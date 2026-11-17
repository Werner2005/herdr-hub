package main

import (
	"net/http"
	"time"
)

// Undeployment retains the database entry, credentials, home and shared mounts.
// Persist intent first so partial cleanup is retryable and workers leave it alone.
func (a *application) undeploy(w http.ResponseWriter, r *http.Request, name string) {
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Status == "deleting" {
		writeError(w, 409, "workspace is deleting")
		return
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET status='undeploying',error='' WHERE name=$1`, name); err != nil {
		writeError(w, 500, "could not save undeployment state")
		return
	}
	if err = a.syncWorkspacePorts(r.Context(), name, true); err != nil {
		writeError(w, 502, "export cleanup incomplete; retry undeploy: "+err.Error())
		return
	}
	for _, kind := range []string{"httproutes", "tcproutes", "deployments", "services"} {
		if err = a.kube.remove(r.Context(), kind, resourceName(name)); err != nil {
			writeError(w, 502, "undeployment incomplete; retry undeploy: "+err.Error())
			return
		}
	}
	// Foreground deletion waits for dependent pods before allowing redeployment.
	for {
		_, status, err := a.kube.call(r.Context(), "GET", "deployments", resourceName(name), nil)
		if status == 404 {
			break
		}
		if err != nil {
			writeError(w, 502, "waiting for runtime shutdown; retry undeploy")
			return
		}
		select {
		case <-r.Context().Done():
			writeError(w, 504, "runtime still stopping; retry undeploy")
			return
		case <-time.After(time.Second):
		}
	}
	if err = a.kube.workspaceListener(r.Context(), item, true); err != nil {
		writeError(w, 502, "listener cleanup incomplete; retry undeploy: "+err.Error())
		return
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET status='undeployed',error='' WHERE name=$1`, name); err != nil {
		writeError(w, 500, "runtime stopped; retry undeploy to save state")
		return
	}
	item.Status, item.Error = "undeployed", ""
	writeJSON(w, 200, item)
}
