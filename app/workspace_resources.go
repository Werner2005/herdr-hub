package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func (a *application) updateWorkspaceResources(w http.ResponseWriter, r *http.Request, name string) {
	var input workspaceResources
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		writeError(w, 400, "invalid workspace resources JSON")
		return
	}
	if err := input.validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if currentUser(r).Role != "admin" {
		limits, err := a.securityLimits(r.Context())
		if err != nil {
			writeError(w, 500, "could not load security limits")
			return
		}
		if err = limits.checkResources(input); err != nil {
			writeError(w, 403, err.Error())
			return
		}
	}
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Status == "deleting" || item.Status == "undeploying" {
		writeError(w, 409, "finish deleting or undeploying the workspace first")
		return
	}
	changed := false
	if item.Status != "undeployed" {
		changed, err = a.kube.updateWorkspaceResources(r.Context(), name, input)
		if err != nil {
			writeError(w, 502, "resource update incomplete; retry saving: "+err.Error())
			return
		}
	}
	if changed {
		item.Status, item.Error = "provisioning", ""
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET resources=$2,status=$3,error=$4 WHERE name=$1`, name, input, item.Status, item.Error); err != nil {
		writeError(w, 500, "runtime may be updated; retry saving resource settings")
		return
	}
	item.Resources = input
	writeJSON(w, 200, item)
}

func sameResourceQuantity(current any, desired string, cpu bool) bool {
	value, ok := current.(string)
	if !ok {
		return false
	}
	if value == desired {
		return true
	}
	a, errA := resourceQuantity(value, cpu)
	b, errB := resourceQuantity(desired, cpu)
	return errA == nil && errB == nil && a.Cmp(b) == 0
}

// Explicit admin edits change only CPU/memory in the herdr container, preserving
// sidecars, extended resources, credentials, PVCs and all other Deployment fields.
func (k *kubernetes) updateWorkspaceResources(ctx context.Context, name string, values workspaceResources) (bool, error) {
	name = resourceName(name)
	for attempt := 0; attempt < 5; attempt++ {
		deployment, status, err := k.call(ctx, "GET", "deployments", name, nil)
		if status == 404 {
			return false, nil
		} // Save settings without recreating a missing runtime.
		if err != nil {
			return false, err
		}
		meta, _ := deployment["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if labels["app.kubernetes.io/managed-by"] != "herdr-hub" || meta["deletionTimestamp"] != nil {
			return false, fmt.Errorf("workspace Deployment is unmanaged or deleting")
		}
		spec, _ := deployment["spec"].(map[string]any)
		strategy, _ := spec["strategy"].(map[string]any)
		if strategy["type"] != "Recreate" {
			return false, fmt.Errorf("workspace must use Recreate")
		}
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
		containers, _ := pod["containers"].([]any)
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			if container["name"] != "herdr" {
				continue
			}
			resources, _ := container["resources"].(map[string]any)
			if resources == nil {
				resources = object{}
				container["resources"] = resources
			}
			changed := false
			for _, group := range []struct{ name, cpu, memory string }{
				{"requests", values.CPURequest, values.MemoryRequest},
				{"limits", values.CPULimit, values.MemoryLimit},
			} {
				quantities, _ := resources[group.name].(map[string]any)
				if quantities == nil {
					quantities = object{}
					resources[group.name] = quantities
				}
				for key, value := range map[string]string{"cpu": group.cpu, "memory": group.memory} {
					if value == "" {
						if _, exists := quantities[key]; exists {
							delete(quantities, key)
							changed = true
						}
					} else if !sameResourceQuantity(quantities[key], value, key == "cpu") {
						quantities[key], changed = value, true
					}
				}
			}
			if !changed {
				return false, nil
			}
			delete(deployment, "status")
			_, status, err = k.call(ctx, "PUT", "deployments", name, deployment)
			if status == 409 {
				break
			} // Re-read to preserve concurrent edits.
			return err == nil, err
		}
		if status != 409 {
			return false, fmt.Errorf("workspace has no herdr container")
		}
	}
	return false, fmt.Errorf("workspace changed concurrently; retry saving resources")
}
