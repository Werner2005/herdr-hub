package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type workspacePort struct {
	Name         string `json:"name"`
	ID           string `json:"-"`
	Protocol     string `json:"protocol"`
	InternalPort int    `json:"internal_port"`
	ExternalPort int    `json:"external_port"`
	Address      string `json:"address"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	Deleting     bool   `json:"deleting"`
}

func (p workspacePort) resource() string { return "herdr-export-" + p.ID }
func (p workspacePort) route() (string, string) {
	switch p.Protocol {
	case "HTTP":
		return "HTTPRoute", "httproutes"
	case "UDP":
		return "UDPRoute", "udproutes"
	default:
		return "TCPRoute", "tcproutes"
	}
}
func validateWorkspacePort(p workspacePort) error {
	if !validName(p.Name) || (p.Protocol != "HTTP" && p.Protocol != "TCP" && p.Protocol != "UDP") || p.InternalPort < 1 || p.InternalPort > 65535 {
		return fmt.Errorf("provide a valid name, HTTP/TCP/UDP protocol and internal port (1–65535)")
	}
	// Do not create alternate access paths to the built-in control services.
	if p.InternalPort == 2222 || p.InternalPort == 7317 {
		return fmt.Errorf("built-in SSH and web ports cannot be exported")
	}
	return nil
}

func (a *application) workspacePorts(ctx context.Context, name string) ([]workspacePort, error) {
	rows, err := a.db.Query(ctx, `SELECT name,id,protocol,internal_port,external_port,status,error,deleting FROM workspace_ports WHERE workspace=$1 ORDER BY name`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []workspacePort{}
	for rows.Next() {
		var p workspacePort
		if err = rows.Scan(&p.Name, &p.ID, &p.Protocol, &p.InternalPort, &p.ExternalPort, &p.Status, &p.Error, &p.Deleting); err != nil {
			return nil, err
		}
		p.Address = a.kube.exportAddress(name, p)
		result = append(result, p)
	}
	return result, rows.Err()
}

func (k *kubernetes) exportAddress(workspace string, p workspacePort) string {
	if p.ExternalPort == 0 {
		return ""
	} // Legacy HTTP export awaiting port allocation.
	address := net.JoinHostPort(workspace+"."+k.domain, strconv.Itoa(p.ExternalPort))
	if p.Protocol == "HTTP" {
		return "https://" + address
	}
	return address
}

func exportPortRange() (int, int, error) {
	start, e1 := strconv.Atoi(env("WORKSPACE_EXPORT_PORT_START", "2300"))
	end, e2 := strconv.Atoi(env("WORKSPACE_EXPORT_PORT_END", "2399"))
	if e1 != nil || e2 != nil || start < 1 || end > 65535 || start > end {
		return 0, 0, fmt.Errorf("invalid export port range")
	}
	return start, end, nil
}

// Both allocators reserve all database allocations, including undeployed ports.
func (a *application) reservedExportPorts(ctx context.Context, used map[int]bool) error {
	rows, err := a.db.Query(ctx, `SELECT external_port FROM workspace_ports WHERE external_port>0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err = rows.Scan(&p); err != nil {
			return err
		}
		used[p] = true
	}
	return rows.Err()
}

func (a *application) allocateExportPort(ctx context.Context) (int, error) {
	start, end, err := exportPortRange()
	if err != nil {
		return 0, err
	}
	g, err := a.kube.get(ctx, "gateways", a.kube.gateway)
	if err != nil {
		return 0, err
	}
	_, listeners, err := gatewayListeners(g)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, raw := range listeners {
		l, _ := raw.(map[string]any)
		port, _ := l["port"].(float64)
		used[int(port)] = true
	}
	for _, p := range a.kube.listeners {
		used[p] = true
	}
	rows, err := a.db.Query(ctx, `SELECT ssh_port FROM workspaces WHERE ssh_port>0`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var p int
		if err = rows.Scan(&p); err != nil {
			rows.Close()
			return 0, err
		}
		used[p] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if err = a.reservedExportPorts(ctx, used); err != nil {
		return 0, err
	}
	for p := start; p <= end; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("export port range exhausted")
}

func (a *application) portsAPI(w http.ResponseWriter, r *http.Request, parts []string) {
	item, err := a.load(r.Context(), parts[1])
	u := currentUser(r)
	if err != nil || (u.Role != "admin" && item.Owner != u.Username) {
		writeError(w, 404, "workspace not found")
		return
	}
	if len(parts) == 3 && r.Method == "GET" {
		writeJSON(w, 200, item.Ports)
		return
	}
	if item.Status == "deleting" || item.Status == "undeploying" {
		writeError(w, 409, "finish workspace cleanup first")
		return
	}
	if len(parts) == 3 && r.Method == "POST" {
		var input struct {
			Name         string `json:"name"`
			Protocol     string `json:"protocol"`
			InternalPort int    `json:"internal_port"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil {
			writeError(w, 400, "invalid port JSON")
			return
		}
		p := workspacePort{Name: input.Name, Protocol: strings.ToUpper(input.Protocol), InternalPort: input.InternalPort}
		if err = validateWorkspacePort(p); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if len(item.Ports) >= 16 {
			writeError(w, 409, "maximum 16 exports per workspace")
			return
		}
		for _, old := range item.Ports {
			if old.Name == p.Name {
				writeError(w, 409, "export name already exists; remove it before replacing")
				return
			}
		}
		id := make([]byte, 12)
		if _, err = rand.Read(id); err != nil {
			writeError(w, 500, "could not generate export ID")
			return
		}
		p.ID = hex.EncodeToString(id)
		p.ExternalPort, err = a.allocateExportPort(r.Context())
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		_, err = a.db.Exec(r.Context(), `INSERT INTO workspace_ports(workspace,name,id,protocol,internal_port,external_port) VALUES($1,$2,$3,$4,$5,$6)`, item.Name, p.Name, p.ID, p.Protocol, p.InternalPort, p.ExternalPort)
		if err != nil {
			writeError(w, 500, "could not save export")
			return
		}
	} else if len(parts) == 4 && validName(parts[3]) && r.Method == "DELETE" {
		result, e := a.db.Exec(r.Context(), `UPDATE workspace_ports SET deleting=true,status='deleting' WHERE workspace=$1 AND name=$2`, item.Name, parts[3])
		if e != nil {
			writeError(w, 500, "could not mark export for deletion")
			return
		}
		if result.RowsAffected() == 0 {
			writeError(w, 404, "export not found")
			return
		}
	} else {
		writeError(w, 405, "method not allowed")
		return
	}
	err = a.syncWorkspacePorts(r.Context(), item.Name, item.Status == "undeployed")
	if err != nil {
		writeError(w, 502, "export saved; reconciliation will retry: "+err.Error())
		return
	}
	if r.Method == "DELETE" {
		w.WriteHeader(204)
		return
	}
	ports, err := a.workspacePorts(r.Context(), item.Name)
	if err != nil {
		writeError(w, 500, "could not load exports")
		return
	}
	writeJSON(w, 201, ports)
}

func (k *kubernetes) exportResources(workspace string, p workspacePort) []object {
	labels := object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/workspace": workspace, "herdr-hub/export": p.ID}
	makeObj := func(api, kind string, spec object) object {
		return object{"apiVersion": api, "kind": kind, "metadata": object{"name": p.resource(), "namespace": k.namespace, "labels": labels}, "spec": spec}
	}
	protocol := "TCP"
	if p.Protocol == "UDP" {
		protocol = "UDP"
	}
	service := makeObj("v1", "Service", object{"selector": object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/workspace": workspace}, "ports": []object{{"name": "export", "protocol": protocol, "port": p.InternalPort, "targetPort": p.InternalPort}}})
	parent := object{"name": k.gateway, "sectionName": p.resource()}
	if k.gatewayNamespace != "" {
		parent["namespace"] = k.gatewayNamespace
	}
	kind, _ := p.route()
	api := "gateway.networking.k8s.io/v1alpha2"
	spec := object{"parentRefs": []object{parent}, "rules": []object{{"backendRefs": []object{{"name": p.resource(), "port": p.InternalPort}}}}}
	if p.Protocol == "HTTP" {
		api = "gateway.networking.k8s.io/v1"
		spec["hostnames"] = []string{workspace + "." + k.domain}
	}
	// Use the same gateway selectors as the chart's base workspace policy.
	pods := object{"gateway.envoyproxy.io/owning-gateway-name": k.gateway, "gateway.envoyproxy.io/owning-gateway-namespace": k.gatewayNamespace}
	if k.gatewayNamespace == "" {
		pods["gateway.envoyproxy.io/owning-gateway-namespace"] = k.namespace
	}
	namespaces := object{}
	if value := os.Getenv("WORKSPACE_GATEWAY_POD_SELECTOR"); value != "" {
		var configured object
		if json.Unmarshal([]byte(value), &configured) == nil && len(configured) > 0 {
			pods = configured
		}
	}
	_ = json.Unmarshal([]byte(env("WORKSPACE_GATEWAY_NAMESPACE_SELECTOR", "{}")), &namespaces)
	policy := makeObj("networking.k8s.io/v1", "NetworkPolicy", object{"podSelector": object{"matchLabels": object{"herdr-hub/workspace": workspace}}, "policyTypes": []string{"Ingress"}, "ingress": []object{{"from": []object{{"namespaceSelector": object{"matchLabels": namespaces}, "podSelector": object{"matchLabels": pods}}}, "ports": []object{{"protocol": protocol, "port": p.InternalPort}}}}})
	if env("WORKSPACE_NETWORK_POLICY_ENABLED", "true") == "false" {
		return []object{service, makeObj(api, kind, spec)}
	}
	return []object{service, policy, makeObj(api, kind, spec)}
}

func (k *kubernetes) cleanupExport(ctx context.Context, p workspacePort) error {
	_, kind := p.route()
	for _, resource := range []string{kind, "services", "networkpolicies"} {
		if err := k.remove(ctx, resource, p.resource()); err != nil {
			return err
		}
		// Keep the database reservation until asynchronous deletion completes.
		for {
			_, status, err := k.call(ctx, "GET", resource, p.resource(), nil)
			if status == 404 {
				break
			}
			if err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if p.ExternalPort == 0 {
		return nil
	} // Legacy HTTP mapping never had a listener.
	return k.exportListener(ctx, "", p, true)
}

func (k *kubernetes) exportListener(ctx context.Context, workspace string, p workspacePort, remove bool) error {
	kind, _ := p.route()
	protocol := p.Protocol
	if protocol == "HTTP" {
		protocol = "HTTPS"
	}
	return k.portListenerForHost(ctx, p.resource(), p.ExternalPort, protocol, kind, remove, workspace+"."+k.domain)
}

// Move legacy generated-host HTTP routes to their dedicated HTTPS listener.
// Preserve backend rules, API-defaulted fields, metadata, and resourceVersion.
func (k *kubernetes) ensureExportResource(ctx context.Context, desired object) error {
	if err := k.ensure(ctx, desired); err != nil {
		return err
	}
	if desired["kind"] != "HTTPRoute" {
		return nil
	}
	metadata := desired["metadata"].(object)
	name := metadata["name"].(string)
	want := desired["spec"].(object)
	parent := want["parentRefs"].([]object)[0]
	for attempt := 0; attempt < 5; attempt++ {
		route, err := k.get(ctx, "httproutes", name)
		if err != nil {
			return err
		}
		meta, _ := route["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		for _, key := range []string{"app.kubernetes.io/managed-by", "herdr-hub/workspace", "herdr-hub/export"} {
			if labels[key] != metadata["labels"].(object)[key] {
				return fmt.Errorf("export route %s is not owned by this workspace export", name)
			}
		}
		if meta["deletionTimestamp"] != nil {
			return fmt.Errorf("export route %s is deleting", name)
		}
		spec, _ := route["spec"].(map[string]any)
		if spec == nil {
			return fmt.Errorf("invalid export route spec")
		}
		hosts, _ := spec["hostnames"].([]any)
		parents, _ := spec["parentRefs"].([]any)
		matches := len(parents) == 1 && len(hosts) == 1 && hosts[0] == want["hostnames"].([]string)[0]
		if len(parents) == 1 {
			ref, _ := parents[0].(map[string]any)
			ns := ref["namespace"]
			if ns == nil {
				ns = k.namespace
			}
			wantNS := parent["namespace"]
			if wantNS == nil {
				wantNS = k.namespace
			}
			matches = matches && ref["name"] == parent["name"] && ref["sectionName"] == parent["sectionName"] && ns == wantNS && (ref["group"] == nil || ref["group"] == "gateway.networking.k8s.io") && (ref["kind"] == nil || ref["kind"] == "Gateway")
		}
		if matches {
			return nil
		}
		spec["hostnames"], spec["parentRefs"] = want["hostnames"], want["parentRefs"]
		delete(route, "status")
		_, status, err := k.call(ctx, "PUT", "httproutes", name, route)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("export route changed concurrently; retry")
}

func routeReadiness(route object, gateway, namespace, section string) (string, string) {
	meta, _ := route["metadata"].(map[string]any)
	status, _ := route["status"].(map[string]any)
	parents, _ := status["parents"].([]any)
	for _, raw := range parents {
		parent, _ := raw.(map[string]any)
		ref, _ := parent["parentRef"].(map[string]any)
		if ref["name"] != gateway || ref["sectionName"] != section || (ref["namespace"] != nil && ref["namespace"] != namespace) {
			continue
		}
		conditions, _ := parent["conditions"].([]any)
		accepted, resolved := false, false
		for _, raw := range conditions {
			c, _ := raw.(map[string]any)
			if c["observedGeneration"] != meta["generation"] {
				continue
			}
			if c["type"] != "Accepted" && c["type"] != "ResolvedRefs" {
				continue
			}
			if c["status"] == "False" {
				message, _ := c["message"].(string)
				return "error", message
			}
			if c["type"] == "Accepted" && c["status"] == "True" {
				accepted = true
			}
			if c["type"] == "ResolvedRefs" && c["status"] == "True" {
				resolved = true
			}
		}
		if accepted && resolved {
			return "ready", ""
		}
	}
	return "pending", "Waiting for gateway route acceptance"
}

// Caller holds the process-wide mutation lock. Never create a missing runtime.
func (a *application) syncWorkspacePorts(ctx context.Context, name string, stop bool) error {
	ports, err := a.workspacePorts(ctx, name)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return nil
	}
	active := !stop
	if active {
		deployment, status, e := a.kube.call(ctx, "GET", "deployments", resourceName(name), nil)
		if status == 404 {
			active = false
		} else if e != nil {
			return e
		} else {
			meta, _ := deployment["metadata"].(map[string]any)
			labels, _ := meta["labels"].(map[string]any)
			if labels["app.kubernetes.io/managed-by"] != "herdr-hub" || labels["herdr-hub/workspace"] != name {
				return fmt.Errorf("workspace deployment is not owned by Herdr Hub")
			}
			if meta["deletionTimestamp"] != nil {
				active = false
			}
		}
	}
	var failures []error
	for _, p := range ports {
		state, message := "inactive", ""
		e := func() error {
			if !active || p.Deleting {
				return a.kube.cleanupExport(ctx, p)
			}
			if p.ExternalPort == 0 {
				// Persist migration allocation before touching Kubernetes, so retries
				// and SSH allocation cannot claim the same port after a failure.
				var err error
				p.ExternalPort, err = a.allocateExportPort(ctx)
				if err != nil {
					return err
				}
				if _, err = a.db.Exec(ctx, `UPDATE workspace_ports SET external_port=$3,status='pending',error='' WHERE workspace=$1 AND name=$2`, name, p.Name, p.ExternalPort); err != nil {
					return err
				}
			}
			if err := a.kube.exportListener(ctx, name, p, false); err != nil {
				return err
			}
			for _, obj := range a.kube.exportResources(name, p) {
				if err := a.kube.ensureExportResource(ctx, obj); err != nil {
					return err
				}
			}
			_, kind := p.route()
			route, err := a.kube.get(ctx, kind, p.resource())
			if err != nil {
				return err
			}
			ns, section := a.kube.gatewayNamespace, p.resource()
			if ns == "" {
				ns = a.kube.namespace
			}
			state, message = routeReadiness(route, a.kube.gateway, ns, section)
			return nil
		}()
		if e != nil {
			state, message = "error", e.Error()
			failures = append(failures, e)
		}
		if p.Deleting && e == nil {
			_, err = a.db.Exec(ctx, `DELETE FROM workspace_ports WHERE workspace=$1 AND name=$2`, name, p.Name)
		} else {
			_, err = a.db.Exec(ctx, `UPDATE workspace_ports SET status=$3,error=$4 WHERE workspace=$1 AND name=$2`, name, p.Name, state, message)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (a *application) runPortUpdates(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		rows, err := a.db.Query(ctx, `SELECT DISTINCT workspace FROM workspace_ports ORDER BY workspace`)
		if err == nil {
			names := []string{}
			for rows.Next() {
				var name string
				if err = rows.Scan(&name); err != nil {
					break
				}
				names = append(names, name)
			}
			rows.Close()
			for _, name := range names {
				func() {
					pass, cancel := context.WithTimeout(ctx, 90*time.Second)
					defer cancel()
					if e := a.mu.lock(pass); e != nil {
						return
					}
					defer a.mu.unlock()
					var status string
					if e := a.db.QueryRow(pass, `SELECT status FROM workspaces WHERE name=$1`, name).Scan(&status); e != nil {
						return
					}
					if e := a.syncWorkspacePorts(pass, name, status == "undeployed" || status == "undeploying" || status == "deleting"); e != nil {
						log.Printf("workspace %s export reconciliation: %v", name, e)
					}
				}()
			}
		} else {
			log.Printf("export reconciliation: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
