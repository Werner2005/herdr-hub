package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Each rule allows one destination and one port, never an implicit allow-all.
type egressRule struct {
	Name        string            `json:"name"`
	CIDR        string            `json:"cidr,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	PodSelector map[string]string `json:"pod_selector,omitempty"`
	Protocol    string            `json:"protocol"`
	Port        int               `json:"port"`
}

type egressSettings struct {
	Rules   []egressRule `json:"rules"`
	Status  string       `json:"status"`
	Error   string       `json:"error,omitempty"`
	Enabled bool         `json:"enabled"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var labelName = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

func validEgressLabelKey(key string) bool {
	parts := strings.Split(key, "/")
	name := parts[len(parts)-1]
	if len(parts) > 2 || len(name) > 63 || !labelName.MatchString(name) {
		return false
	}
	if len(parts) == 2 {
		if len(parts[0]) > 253 {
			return false
		}
		for _, label := range strings.Split(parts[0], ".") {
			if len(label) > 63 || !dnsLabel.MatchString(label) {
				return false
			}
		}
	}
	return true
}

func validateEgressRules(rules []egressRule) error {
	if len(rules) > 32 {
		return fmt.Errorf("maximum 32 egress rules per scope")
	}
	names := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		r.Protocol = strings.ToUpper(r.Protocol)
		if !validName(r.Name) || names[r.Name] {
			return fmt.Errorf("rule names must be valid and unique")
		}
		names[r.Name] = true
		if (r.Protocol != "TCP" && r.Protocol != "UDP") || r.Port < 1 || r.Port > 65535 {
			return fmt.Errorf("each rule requires TCP or UDP and a port from 1 to 65535")
		}
		if r.CIDR != "" {
			if r.Namespace != "" || len(r.PodSelector) != 0 {
				return fmt.Errorf("choose either an IP/CIDR or namespace and pod labels")
			}
			prefix, err := netip.ParsePrefix(r.CIDR)
			if err != nil {
				addr, e := netip.ParseAddr(r.CIDR)
				if e != nil || addr.Zone() != "" {
					return fmt.Errorf("invalid destination IP/CIDR; hostnames are not supported")
				}
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			}
			if prefix.Addr().Is4In6() {
				return fmt.Errorf("use an IPv4 address instead of IPv4-mapped IPv6")
			}
			r.CIDR = prefix.Masked().String()
		} else {
			if len(r.Namespace) > 63 || !dnsLabel.MatchString(r.Namespace) || len(r.PodSelector) == 0 || len(r.PodSelector) > 8 {
				return fmt.Errorf("pod destinations require a namespace and 1–8 pod labels")
			}
			for key, value := range r.PodSelector {
				if !validEgressLabelKey(key) || len(value) > 63 || (value != "" && !labelName.MatchString(value)) {
					return fmt.Errorf("invalid Kubernetes pod label")
				}
			}
		}
	}
	return nil
}

func egressPolicyName(scope string) string {
	if scope == "" {
		return "herdr-egress-defaults"
	}
	return "herdr-egress-w-" + scope
}

func (a *application) egressSettings(ctx context.Context, scope string) (egressSettings, error) {
	s := egressSettings{Rules: []egressRule{}, Status: "ready", Enabled: env("WORKSPACE_NETWORK_POLICY_ENABLED", "true") == "true"}
	query, args := `SELECT rules,status,error FROM hub_egress_defaults WHERE id=1`, []any{}
	if scope != "" {
		query, args = `SELECT rules,status,error FROM workspace_egress WHERE workspace=$1`, []any{scope}
	}
	err := a.db.QueryRow(ctx, query, args...).Scan(&s.Rules, &s.Status, &s.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return s, err
}

// An empty scope is the admin-managed default. Workspace scope is always taken
// from the URL, never from user-supplied rule labels or selectors.
func (a *application) egressAPI(w http.ResponseWriter, r *http.Request, scope string) {
	u := currentUser(r)
	if scope != "" {
		var owner, status string
		err := a.db.QueryRow(r.Context(), `SELECT owner,status FROM workspaces WHERE name=$1`, scope).Scan(&owner, &status)
		if err != nil || (u.Role != "admin" && owner != u.Username) {
			writeError(w, 404, "workspace not found")
			return
		}
		if r.Method != "GET" && (status == "deleting" || status == "undeploying") {
			writeError(w, 409, "finish workspace cleanup first")
			return
		}
	} else if r.Method != "GET" && u.Role != "admin" {
		writeError(w, 403, "admin required for default egress rules")
		return
	}
	if r.Method == "GET" {
		s, err := a.egressSettings(r.Context(), scope)
		if err != nil {
			writeError(w, 500, "could not load egress rules")
			return
		}
		writeJSON(w, 200, s)
		return
	}
	if r.Method != "PUT" {
		writeError(w, 405, "method not allowed")
		return
	}
	if env("WORKSPACE_NETWORK_POLICY_ENABLED", "true") != "true" {
		writeError(w, 409, "workspace network policies are disabled")
		return
	}
	var input struct {
		Rules []egressRule `json:"rules"`
	}
	if !decodeShared(w, r, &input) {
		return
	}
	if input.Rules == nil {
		writeError(w, 400, "provide a rules array; use [] to remove all exceptions")
		return
	}
	if err := validateEgressRules(input.Rules); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	query, args := `INSERT INTO hub_egress_defaults(id,rules,status,error) VALUES(1,$1,'pending','') ON CONFLICT(id) DO UPDATE SET rules=$1,status='pending',error=''`, []any{input.Rules}
	if scope != "" {
		query, args = `INSERT INTO workspace_egress(workspace,rules,status,error) VALUES($1,$2,'pending','') ON CONFLICT(workspace) DO UPDATE SET rules=$2,status='pending',error=''`, []any{scope, input.Rules}
	}
	if _, err := a.db.Exec(r.Context(), query, args...); err != nil {
		writeError(w, 500, "could not save egress rules")
		return
	}
	if err := a.syncEgress(r.Context(), scope); err != nil {
		writeError(w, 502, "rules saved; reconciliation will retry: "+err.Error())
		return
	}
	s, err := a.egressSettings(r.Context(), scope)
	if err != nil {
		writeError(w, 500, "rules saved but could not reload status")
		return
	}
	writeJSON(w, 200, s)
}

func (k *kubernetes) egressPolicy(scope string, rules []egressRule) object {
	selector := object{"matchExpressions": []object{{"key": "herdr-hub/workspace", "operator": "Exists"}}}
	if scope != "" {
		selector = object{"matchLabels": object{"herdr-hub/workspace": scope}}
	}
	entries := []object{}
	for _, r := range rules {
		peer := object{"ipBlock": object{"cidr": r.CIDR}}
		if r.CIDR == "" {
			peer = object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": r.Namespace}}, "podSelector": object{"matchLabels": r.PodSelector}}
		}
		entries = append(entries, object{"to": []object{peer}, "ports": []object{{"protocol": r.Protocol, "port": r.Port}}})
	}
	return object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": object{"name": egressPolicyName(scope), "namespace": k.namespace, "labels": object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/egress-scope": egressPolicyName(scope)}}, "spec": object{"podSelector": selector, "policyTypes": []string{"Egress"}, "egress": entries}}
}

// Update only this policy's spec; preserve metadata and use resourceVersion for
// concurrency control. Never touch Deployments, credentials, PVCs or ingress.
func (k *kubernetes) saveEgressPolicy(ctx context.Context, scope string, rules []egressRule) error {
	desired := k.egressPolicy(scope, rules)
	name := egressPolicyName(scope)
	for attempt := 0; attempt < 5; attempt++ {
		current, status, err := k.call(ctx, "GET", "networkpolicies", name, nil)
		if status == 404 {
			if len(rules) == 0 {
				return nil
			}
			_, status, err = k.call(ctx, "POST", "networkpolicies", "", desired)
			if status == 409 {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		meta, _ := current["metadata"].(object)
		labels, _ := meta["labels"].(object)
		if labels["app.kubernetes.io/managed-by"] != "herdr-hub" || labels["herdr-hub/egress-scope"] != name {
			return fmt.Errorf("egress policy %s is not owned by this scope", name)
		}
		if meta["deletionTimestamp"] != nil {
			return fmt.Errorf("egress policy %s is deleting; retry shortly", name)
		}
		if len(rules) == 0 {
			// Guard deletion against a concurrent replacement of the policy.
			_, status, err = k.call(ctx, "DELETE", "networkpolicies", name, object{"preconditions": object{"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]}})
			if status == 409 {
				continue
			}
			if status == 404 {
				return nil
			}
			if err != nil {
				return err
			}
			// Keep desired state until asynchronous deletion really completes.
			_, status, err = k.call(ctx, "GET", "networkpolicies", name, nil)
			if status == 404 {
				return nil
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("egress policy deletion still pending")
		}
		oldSpec, _ := json.Marshal(current["spec"])
		newSpec, _ := json.Marshal(desired["spec"])
		if string(oldSpec) == string(newSpec) {
			return nil
		}
		current["spec"] = desired["spec"]
		delete(current, "status")
		_, status, err = k.call(ctx, "PUT", "networkpolicies", name, current)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("egress policy changed concurrently; retry")
}

// Caller holds the process-wide mutation lock. Persist failures for the GUI and
// retry saved desired state at startup and every minute, including deletions.
func (a *application) syncEgress(ctx context.Context, scope string) error {
	s, err := a.egressSettings(ctx, scope)
	if err != nil {
		return err
	}
	if !s.Enabled {
		// A leftover additive policy would itself isolate pods after Helm removes
		// the baseline. Remove it, but retain desired rules for re-enabling.
		s.Rules = nil
	}
	if scope != "" {
		var status string
		if err = a.db.QueryRow(ctx, `SELECT status FROM workspaces WHERE name=$1`, scope).Scan(&status); err != nil {
			return err
		}
		if status == "deleting" {
			s.Rules = nil
		}
	}
	err = a.kube.saveEgressPolicy(ctx, scope, s.Rules)
	state, message := "ready", ""
	if !s.Enabled {
		state = "inactive"
	}
	if err != nil {
		state, message = "error", err.Error()
	}
	query, args := `UPDATE hub_egress_defaults SET status=$1,error=$2 WHERE id=1`, []any{state, message}
	if scope != "" {
		query, args = `UPDATE workspace_egress SET status=$2,error=$3 WHERE workspace=$1`, []any{scope, state, message}
	}
	_, dbErr := a.db.Exec(ctx, query, args...)
	return errors.Join(err, dbErr)
}

func (a *application) runEgressUpdates(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		func() {
			pass, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if err := a.mu.lock(pass); err != nil {
				return
			}
			defer a.mu.unlock()
			rows, err := a.db.Query(pass, `SELECT '' FROM hub_egress_defaults UNION ALL SELECT workspace FROM workspace_egress ORDER BY 1`)
			if err != nil {
				log.Printf("egress reconciliation: %v", err)
				return
			}
			scopes := []string{}
			for rows.Next() {
				var scope string
				if err = rows.Scan(&scope); err != nil {
					break
				}
				scopes = append(scopes, scope)
			}
			rows.Close()
			if err != nil || rows.Err() != nil {
				log.Printf("could not read egress scopes: %v %v", err, rows.Err())
				return
			}
			for _, scope := range scopes {
				if err := a.syncEgress(pass, scope); err != nil {
					log.Printf("egress %q reconciliation: %v", scope, err)
				}
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
