package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
)

func storageGi(s string) (int, error) {
	if !regexp.MustCompile(`^[1-9][0-9]*Gi$`).MatchString(s) {
		return 0, fmt.Errorf("storage size must be a positive whole Gi value, e.g. 20Gi")
	}
	n, err := strconv.Atoi(s[:len(s)-2])
	if err != nil || n > 1048576 {
		return 0, fmt.Errorf("storage size is too large")
	}
	return n, nil
}

func (a *application) assignOwner(w http.ResponseWriter, r *http.Request, name string) {
	var input struct {
		Owner string `json:"owner"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil || input.Owner == "" {
		writeError(w, 400, "provide an owner")
		return
	}
	var exists bool
	if err := a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM hub_users WHERE username=$1)`, input.Owner).Scan(&exists); err != nil || !exists {
		writeError(w, 400, "owner must be an existing user")
		return
	}
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Owner != input.Owner && len(item.Volumes) > 0 {
		writeError(w, 409, "detach shared volumes before reassigning the workspace")
		return
	}
	if item.Owner != input.Owner && len(item.Configurations) > 0 {
		writeError(w, 409, "detach private configurations before reassigning the workspace; retained home files may contain copied secrets")
		return
	}
	if item.Owner != input.Owner {
		egress, e := a.egressSettings(r.Context(), name)
		if e != nil {
			writeError(w, 500, "could not load workspace egress rules")
			return
		}
		if len(egress.Rules) > 0 {
			writeError(w, 409, "remove workspace-specific egress rules before reassigning the workspace")
			return
		}
		if e = a.kube.saveEgressPolicy(r.Context(), name, nil); e != nil {
			writeError(w, 502, "finish egress cleanup before reassigning: "+e.Error())
			return
		}
	}
	if item.Owner != input.Owner {
		busy, e := a.kube.workspaceHasSharedPods(r.Context(), name)
		if e != nil {
			writeError(w, 502, "could not inspect workspace pods")
			return
		}
		if busy {
			writeError(w, 409, "wait for old shared-volume pods to terminate before reassigning")
			return
		}
		if e = a.stopForCredentialChange(r.Context(), item); e != nil {
			writeError(w, 502, "could not stop workspace; retry transfer: "+e.Error())
			return
		}
		if e = a.kube.rotateToken(r.Context(), name, true); e != nil {
			writeError(w, 502, "workspace stopped; retry transfer: "+e.Error())
			return
		}
	}
	result, err := a.db.Exec(r.Context(), `UPDATE workspaces SET owner=$2,ssh_public_key=CASE WHEN owner<>$2 THEN '' ELSE ssh_public_key END WHERE name=$1`, name, input.Owner)
	if err != nil {
		writeError(w, 500, "could not assign owner")
		return
	}
	if result.RowsAffected() == 0 {
		writeError(w, 404, "workspace not found")
		return
	}
	writeJSON(w, 200, map[string]string{"owner": input.Owner})
}
func gatewayListeners(g object) (map[string]any, []any, error) {
	spec, ok := g["spec"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("Gateway spec missing")
	}
	listeners, ok := spec["listeners"].([]any)
	if !ok {
		return nil, nil, fmt.Errorf("Gateway listeners missing")
	}
	return spec, listeners, nil
}

func (a *application) upgradeSSH(ctx context.Context) error {
	rows, err := a.db.Query(ctx, `SELECT name FROM workspaces WHERE ssh_port=0 AND status NOT IN ('deleting','undeploying','undeployed')`)
	if err != nil {
		return err
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, name := range names {
		item, err := a.load(ctx, name)
		if err != nil {
			return err
		}
		item.SSHPort, err = a.allocateSSH(ctx)
		if err != nil {
			return err
		}
		item.SSHListener = resourceName(name)
		if _, err = a.db.Exec(ctx, `UPDATE workspaces SET ssh_port=$2,ssh_listener=$3 WHERE name=$1`, name, item.SSHPort, item.SSHListener); err != nil {
			return err
		}
		err = a.kube.workspaceListener(ctx, item, false)
		if err == nil {
			for _, obj := range a.kube.resources(item, "") {
				if obj["kind"] == "TCPRoute" {
					err = a.kube.ensure(ctx, obj)
					break
				}
			}
		}
		if err != nil {
			_, _ = a.db.Exec(ctx, `UPDATE workspaces SET status='error',error=$2 WHERE name=$1`, name, err.Error())
			return err
		}
	}
	return nil
}
func (a *application) allocateSSH(ctx context.Context) (int, error) {
	used := map[int]bool{}
	g, err := a.kube.get(ctx, "gateways", a.kube.gateway)
	if err != nil {
		return 0, err
	}
	_, listeners, err := gatewayListeners(g)
	if err != nil {
		return 0, err
	}
	for _, raw := range listeners {
		l, _ := raw.(map[string]any)
		p, _ := l["port"].(float64)
		used[int(p)] = true
	}
	rows, err := a.db.Query(ctx, `SELECT ssh_port FROM workspaces WHERE ssh_port>0`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err = rows.Scan(&p); err != nil {
			return 0, err
		}
		used[p] = true
	}
	if rows.Err() != nil {
		return 0, rows.Err()
	}
	if err = a.reservedExportPorts(ctx, used); err != nil {
		return 0, err
	}
	start, end := a.kube.sshPortStart, a.kube.sshPortEnd
	if start == 0 {
		start, end = 2201, 2299
	}
	for p := start; p <= end; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("SSH port range exhausted")
}

// Read-modify-write preserves other listeners and uses resourceVersion to guard
// against concurrent Gateway edits. Only workspace-named listeners are managed.
func (k *kubernetes) workspaceListener(ctx context.Context, w workspace, remove bool) error {
	if w.SSHListener != resourceName(w.Name) {
		return nil
	} // Legacy static listeners remain administrator-owned.
	return k.portListener(ctx, w.SSHListener, w.SSHPort, "TCP", "TCPRoute", remove)
}

func (k *kubernetes) portListener(ctx context.Context, name string, port int, protocol, routeKind string, remove bool) error {
	return k.portListenerForHost(ctx, name, port, protocol, routeKind, remove, "")
}

// HTTPS exports reuse the web listener's certificate configuration, but have
// their own port and workspace hostname. Refresh TLS settings without touching
// the web listener or unrelated Gateway fields.
func (k *kubernetes) portListenerForHost(ctx context.Context, name string, port int, protocol, routeKind string, remove bool, hostname string) error {
	for attempt := 0; attempt < 5; attempt++ {
		g, err := k.get(ctx, "gateways", k.gateway)
		if err != nil {
			return err
		}
		spec, listeners, err := gatewayListeners(g)
		if err != nil {
			return err
		}
		result := []any{}
		found := false
		changed := false
		extra := object{}
		if protocol == "HTTPS" && !remove {
			for _, raw := range listeners {
				listener, _ := raw.(map[string]any)
				if listener["name"] != k.webListener || listener["protocol"] != "HTTPS" {
					continue
				}
				tls, _ := listener["tls"].(map[string]any)
				refs, _ := tls["certificateRefs"].([]any)
				if len(refs) == 0 || (tls["mode"] != nil && tls["mode"] != "Terminate") {
					continue
				}
				extra = object{"hostname": hostname, "tls": tls}
			}
			if len(extra) == 0 {
				return fmt.Errorf("HTTP exports require a terminating HTTPS web listener with a certificate")
			}
		}
		for _, raw := range listeners {
			l, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid Gateway listener")
			}
			if l["name"] == name {
				if l["protocol"] != protocol || l["port"] != float64(port) {
					return fmt.Errorf("managed listener conflicts with Gateway configuration")
				}
				found = true
				if remove {
					continue
				}
				for key, value := range extra {
					if !reflect.DeepEqual(l[key], value) {
						l[key] = value
						changed = true
					}
				}
			} else if !remove && l["port"] == float64(port) {
				return fmt.Errorf("port already used on Gateway")
			}
			result = append(result, raw)
		}
		if (remove && !found) || (!remove && found && !changed) {
			return nil
		}
		if !remove && !found {
			if len(result) >= 64 {
				return fmt.Errorf("Gateway listener limit (64) reached")
			}
			namespaces := object{"from": "Same"}
			if k.gatewayNamespace != "" && k.gatewayNamespace != k.namespace {
				namespaces = object{"from": "Selector", "selector": object{"matchLabels": object{"kubernetes.io/metadata.name": k.namespace}}}
			}
			listener := object{"name": name, "protocol": protocol, "port": port, "allowedRoutes": object{"kinds": []object{{"kind": routeKind}}, "namespaces": namespaces}}
			for key, value := range extra {
				listener[key] = value
			}
			result = append(result, listener)
		}
		spec["listeners"] = result
		delete(g, "status")
		_, status, err := k.call(ctx, "PUT", "gateways", k.gateway, g)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("Gateway changed concurrently; retry")
}
func (a *application) resize(w http.ResponseWriter, r *http.Request, name string) {
	var input struct {
		Size string `json:"storage_size"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil {
		writeError(w, 400, "invalid size JSON")
		return
	}
	requested, err := storageGi(input.Size)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Status == "deleting" {
		writeError(w, 409, "workspace is deleting")
		return
	}
	obj, err := a.kube.get(r.Context(), "persistentvolumeclaims", resourceName(name))
	if err != nil {
		writeError(w, 502, "home volume unavailable")
		return
	}
	meta, _ := obj["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	if labels["app.kubernetes.io/managed-by"] != "herdr-hub" {
		writeError(w, 409, "home volume is not owned by Herdr Hub")
		return
	}
	spec, _ := obj["spec"].(map[string]any)
	resources, _ := spec["resources"].(map[string]any)
	requests, _ := resources["requests"].(map[string]any)
	size, _ := requests["storage"].(string)
	old, err := storageGi(size)
	if err != nil {
		writeError(w, 409, "existing home volume must use whole Gi sizes")
		return
	}
	if requested < old {
		writeError(w, 400, "home volumes cannot shrink")
		return
	}
	if requested > old {
		requests["storage"] = input.Size
		delete(obj, "status")
		if _, _, err = a.kube.call(r.Context(), "PUT", "persistentvolumeclaims", resourceName(name), obj); err != nil {
			writeError(w, 502, "volume expansion failed (StorageClass must allow expansion): "+err.Error())
			return
		}
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET storage_size=$2 WHERE name=$1`, name, input.Size); err != nil {
		writeError(w, 500, "volume updated; retry to save database size")
		return
	}
	item.StorageSize = input.Size
	writeJSON(w, 200, item)
}
