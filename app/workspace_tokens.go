package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

// Stop all old processes before changing credentials. Persist the stop intent
// first so image reconciliation cannot recreate or update the retiring runtime.
func (a *application) stopForCredentialChange(ctx context.Context, item workspace) error {
	if err := a.childReferences(ctx, item.Name); err != nil {
		return err
	}
	if _, err := a.db.Exec(ctx, `DELETE FROM hub_child_tokens WHERE parent=$1`, item.Name); err != nil {
		return err
	}
	if item.Status == "deleting" {
		return fmt.Errorf("workspace is deleting")
	}
	if _, err := a.db.Exec(ctx, `UPDATE workspaces SET status='undeploying' WHERE name=$1`, item.Name); err != nil {
		return err
	}
	if err := a.syncWorkspacePorts(ctx, item.Name, true); err != nil {
		return err
	}
	for _, kind := range []string{"httproutes", "tcproutes", "deployments", "services"} {
		if err := a.kube.remove(ctx, kind, resourceName(item.Name)); err != nil {
			return err
		}
	}
	for {
		_, status, err := a.kube.call(ctx, "GET", "deployments", resourceName(item.Name), nil)
		if err != nil && status != 404 {
			return err
		}
		pods, err := a.kube.sharedPods(ctx)
		if err != nil {
			return err
		}
		found := status != 404
		for _, raw := range pods {
			pod, _ := raw.(map[string]any)
			meta, _ := pod["metadata"].(map[string]any)
			labels, _ := meta["labels"].(map[string]any)
			if labels["herdr-hub/workspace"] == item.Name {
				found = true
			}
		}
		if !found {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err := a.kube.workspaceListener(ctx, item, true); err != nil {
		return err
	}
	_, err := a.db.Exec(ctx, `UPDATE workspaces SET status='undeployed',error='' WHERE name=$1`, item.Name)
	return err
}

func (k *kubernetes) rotateToken(ctx context.Context, name string, clearSSH bool) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	token := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(b)))
	for attempt := 0; attempt < 5; attempt++ {
		secret, err := k.get(ctx, "secrets", resourceName(name))
		if err != nil {
			return err
		}
		meta, _ := secret["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if meta["deletionTimestamp"] != nil || labels["app.kubernetes.io/managed-by"] != "herdr-hub" || labels["herdr-hub/workspace"] != name {
			return fmt.Errorf("workspace Secret is unmanaged or deleting")
		}
		data, ok := secret["data"].(map[string]any)
		if !ok {
			return fmt.Errorf("workspace Secret data missing")
		}
		data["token"] = token
		if clearSSH {
			data["authorized_keys"] = base64.StdEncoding.EncodeToString([]byte("\n"))
		}
		delete(secret, "status")
		_, status, err := k.call(ctx, "PUT", "secrets", resourceName(name), secret)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("workspace Secret changed concurrently; retry")
}

func (a *application) rotateWorkspaceToken(w http.ResponseWriter, r *http.Request, name string) {
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if err = a.stopForCredentialChange(r.Context(), item); err != nil {
		writeError(w, 502, "could not stop workspace; retry rotation: "+err.Error())
		return
	}
	if err = a.kube.rotateToken(r.Context(), name, false); err != nil {
		writeError(w, 502, "workspace stopped; retry rotation: "+err.Error())
		return
	}
	// Redeployment is explicit: this also makes retries after partial failure safe.
	writeJSON(w, 200, map[string]string{"status": "undeployed"})
}
