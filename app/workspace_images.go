package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Image upgrades are separate from create-only provisioning retries. Update the
// existing Deployment in place, preserving its PVC, credentials and custom spec.
func (k *kubernetes) updateWorkspaceImage(ctx context.Context, name string) (bool, error) {
	name = resourceName(name)
	for attempt := 0; attempt < 5; attempt++ {
		deployment, status, err := k.call(ctx, "GET", "deployments", name, nil)
		if status == 404 {
			return false, nil // Do not recreate a deleted or partially provisioned runtime.
		}
		if err != nil {
			return false, err
		}
		meta, _ := deployment["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if labels["app.kubernetes.io/managed-by"] != "herdr-hub" {
			return false, fmt.Errorf("resource %s is not owned by Herdr Hub", name)
		}
		if meta["deletionTimestamp"] != nil {
			return false, nil
		}
		spec, _ := deployment["spec"].(map[string]any)
		strategy, _ := spec["strategy"].(map[string]any)
		if strategy["type"] != "Recreate" {
			return false, fmt.Errorf("workspace %s must use Recreate before updating its image", name)
		}
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
		containers, _ := pod["containers"].([]any)
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			if container["name"] != "herdr" {
				continue
			}
			policy := env("WORKSPACE_IMAGE_PULL_POLICY", "IfNotPresent")
			if container["image"] == k.image && container["imagePullPolicy"] == policy {
				return false, nil
			}
			container["image"] = k.image
			container["imagePullPolicy"] = policy
			delete(deployment, "status")
			// Keep resourceVersion so concurrent edits fail instead of being lost.
			_, status, err = k.call(ctx, "PUT", "deployments", name, deployment)
			if status == 409 {
				break // Re-read the latest spec before retrying.
			}
			return err == nil, err
		}
		if status != 409 {
			return false, fmt.Errorf("workspace %s has no herdr container", name)
		}
	}
	return false, fmt.Errorf("workspace %s changed concurrently; retrying next image update pass", name)
}

func (a *application) reconcileWorkspaceImages(ctx context.Context) error {
	rows, err := a.db.Query(ctx, `SELECT name FROM workspaces WHERE status NOT IN ('deleting','undeploying','undeployed') ORDER BY name`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var failures []error
	for _, name := range names {
		// Serialize each update with API mutations, and recheck the database after
		// taking the lock so a queued deletion is never undone by this worker.
		err := func() error {
			ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			if err := a.mu.lock(ctx); err != nil {
				return err
			}
			defer a.mu.unlock()
			var exists bool
			if err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE name=$1 AND status NOT IN ('deleting','undeploying','undeployed'))`, name).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return nil
			}
			changed, err := a.kube.updateWorkspaceImage(ctx, name)
			if changed {
				log.Printf("workspace %s rolling out image %s", name, a.kube.image)
			}
			return err
		}()
		if err != nil {
			failures = append(failures, fmt.Errorf("workspace %s: %w", name, err))
		}
	}
	return errors.Join(failures...)
}

func (a *application) runWorkspaceImageUpdates(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if err := a.reconcileWorkspaceImages(passCtx); err != nil {
			log.Printf("workspace image update needs retry: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
