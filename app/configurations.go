package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type configurationFile struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}
type configurationEnv struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type configurationMapping struct {
	Configuration string              `json:"configuration"`
	Files         []configurationFile `json:"files"`
	Env           []configurationEnv  `json:"env"`
}
type configuration struct {
	Name    string               `json:"name"`
	Owner   string               `json:"owner"`
	Kind    string               `json:"kind"`
	Global  bool                 `json:"global"`
	Mapping configurationMapping `json:"mapping"`
	Keys    []string             `json:"keys"`
	Version string               `json:"-"`
	Ready   bool                 `json:"ready"`
}
type resolvedConfiguration struct {
	Configuration configuration
	Mapping       configurationMapping
}

var configurationKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var configurationEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func configurationResource(name string) string { return "herdr-config-" + name }
func configurationKind(kind string) string {
	if kind == "secret" {
		return "secrets"
	}
	return "configmaps"
}
func validConfigurationPath(p string) bool {
	if len(p) > 512 || strings.ContainsAny(p, "\x00\r\n") || path.Clean(p) != p {
		return false
	}
	allowed := strings.HasPrefix(p, "/home/dev/") || strings.HasPrefix(p, "/mnt/config/") || strings.HasPrefix(p, "/mnt/secrets/")
	for _, reserved := range []string{"/home/dev/.ssh", "/home/dev/.local", "/home/dev/.bun", "/home/dev/.config/herdr", "/home/dev/.config/herdr-web-ui", "/home/dev/.config/ovis"} {
		if p == reserved || strings.HasPrefix(p, reserved+"/") || strings.HasPrefix(reserved, p+"/") {
			return false
		}
	}
	return allowed && p != "/home/dev/workspace"
}
func validConfigurationEnv(name string) bool {
	if len(name) > 128 || !configurationEnvPattern.MatchString(name) {
		return false
	}
	for _, prefix := range []string{"HERDR_", "OVIS_", "XDG_", "LD_", "BASH_"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return !slices.Contains([]string{"HOST", "PORT", "HOME", "PATH", "SHELL", "USER", "LOGNAME", "ENV", "BUN_OPTIONS"}, name)
}
func validateConfigurationMappings(items []resolvedConfiguration) error {
	files, envs := []string{}, map[string]bool{}
	count := 0
	for _, item := range items {
		m, c := item.Mapping, item.Configuration
		if !c.Ready {
			return fmt.Errorf("configuration %s is not ready; retry saving it", c.Name)
		}
		if len(m.Files)+len(m.Env) == 0 {
			return fmt.Errorf("configuration %s needs a file or environment mapping", c.Name)
		}
		for _, f := range m.Files {
			count++
			if !slices.Contains(c.Keys, f.Key) || !validConfigurationPath(f.Path) {
				return fmt.Errorf("invalid key or file path for configuration %s", c.Name)
			}
			for _, previous := range files {
				if previous == f.Path || strings.HasPrefix(previous, f.Path+"/") || strings.HasPrefix(f.Path, previous+"/") {
					return fmt.Errorf("configuration file paths overlap")
				}
			}
			files = append(files, f.Path)
		}
		for _, e := range m.Env {
			count++
			if !slices.Contains(c.Keys, e.Key) || !validConfigurationEnv(e.Name) || envs[e.Name] {
				return fmt.Errorf("invalid, reserved or duplicate environment variable %s", e.Name)
			}
			envs[e.Name] = true
		}
	}
	if count > 64 {
		return fmt.Errorf("at most 64 file and environment mappings per workspace")
	}
	return nil
}

func (a *application) configurationCatalog(ctx context.Context) ([]configuration, error) {
	rows, err := a.db.Query(ctx, `SELECT name,owner,kind,global,mapping,keys,version,ready FROM configurations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []configuration{}
	for rows.Next() {
		var c configuration
		if err = rows.Scan(&c.Name, &c.Owner, &c.Kind, &c.Global, &c.Mapping, &c.Keys, &c.Version, &c.Ready); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}
func (a *application) workspaceConfigurationMappings(ctx context.Context, name string) ([]configurationMapping, error) {
	rows, err := a.db.Query(ctx, `SELECT mapping FROM workspace_configurations WHERE workspace=$1 ORDER BY configuration`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []configurationMapping{}
	for rows.Next() {
		var m configurationMapping
		if err = rows.Scan(&m); err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}
func resolveConfigurationCatalog(catalog []configuration, owner string, mappings []configurationMapping) ([]resolvedConfiguration, error) {
	items := []resolvedConfiguration{}
	seen := map[string]bool{}
	for _, c := range catalog {
		if c.Global {
			items = append(items, resolvedConfiguration{c, c.Mapping})
		}
	}
	if len(mappings) > 16 {
		return nil, fmt.Errorf("at most 16 private configurations per workspace")
	}
	for _, m := range mappings {
		if seen[m.Configuration] {
			return nil, fmt.Errorf("duplicate configuration")
		}
		seen[m.Configuration] = true
		found := false
		for _, c := range catalog {
			if c.Name == m.Configuration && c.Owner == owner && !c.Global {
				items = append(items, resolvedConfiguration{c, m})
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("workspace owner does not own private configuration %s", m.Configuration)
		}
	}
	if err := validateConfigurationMappings(items); err != nil {
		return nil, err
	}
	// PUT order and database order must produce the same pod and revision; an
	// otherwise equivalent worker pass must not disconnect sessions a second time.
	slices.SortFunc(items, func(a, b resolvedConfiguration) int {
		return strings.Compare(a.Configuration.Name, b.Configuration.Name)
	})
	return items, nil
}
func (a *application) resolveConfigurations(ctx context.Context, owner string, mappings []configurationMapping) ([]resolvedConfiguration, error) {
	catalog, err := a.configurationCatalog(ctx)
	if err != nil {
		return nil, err
	}
	return resolveConfigurationCatalog(catalog, owner, mappings)
}

func (a *application) configurationsAPI(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) > 2 || (len(parts) == 2 && !validName(parts[1])) {
		writeError(w, 404, "endpoint not found")
		return
	}
	catalog, err := a.configurationCatalog(r.Context())
	if err != nil {
		writeError(w, 500, "could not load configurations")
		return
	}
	u := currentUser(r)
	if len(parts) == 1 && r.Method == "GET" {
		visible := []configuration{}
		for _, c := range catalog {
			if u.Role == "admin" || c.Owner == u.Username || c.Global {
				visible = append(visible, c)
			}
		}
		writeJSON(w, 200, visible)
		return
	}
	var existing *configuration
	if len(parts) == 2 {
		for _, c := range catalog {
			if c.Name == parts[1] {
				existing = &c
				break
			}
		}
		if existing == nil || (u.Role != "admin" && existing.Owner != u.Username) {
			writeError(w, 404, "configuration not found")
			return
		}
		if existing.Global && u.Role != "admin" {
			writeError(w, 403, "admin required for global configurations")
			return
		}
	}
	if existing != nil && r.Method == "DELETE" {
		if err = a.deleteConfiguration(r.Context(), *existing); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		w.WriteHeader(204)
		return
	}
	if !(len(parts) == 1 && r.Method == "POST") && !(existing != nil && r.Method == "PUT") {
		writeError(w, 404, "endpoint not found")
		return
	}
	var input struct {
		Name    string               `json:"name"`
		Owner   string               `json:"owner"`
		Kind    string               `json:"kind"`
		Global  bool                 `json:"global"`
		Mapping configurationMapping `json:"mapping"`
		Data    map[string]string    `json:"data"`
	}
	if !decodeShared(w, r, &input) {
		return
	}
	c := configuration{Name: input.Name, Owner: u.Username, Kind: input.Kind, Global: input.Global, Mapping: input.Mapping, Ready: true, Keys: []string{}}
	if input.Owner != "" {
		c.Owner = input.Owner
	}
	if existing != nil {
		if !existing.Ready && input.Data == nil {
			writeError(w, 400, "provide all data values to retry an incomplete configuration save")
			return
		}
		c.Name, c.Owner, c.Kind, c.Keys = existing.Name, existing.Owner, existing.Kind, existing.Keys
		if (input.Name != "" && input.Name != c.Name) || (input.Kind != "" && input.Kind != c.Kind) || (input.Owner != "" && input.Owner != c.Owner) {
			writeError(w, 400, "name, kind and owner cannot change")
			return
		}
	} else {
		if u.Role != "admin" && c.Owner != u.Username {
			writeError(w, 403, "create configurations for your own account")
			return
		}
		if !validName(c.Name) || (c.Kind != "configmap" && c.Kind != "secret") || input.Data == nil {
			writeError(w, 400, "provide name, kind (configmap or secret), and data")
			return
		}
		owned := 0
		for _, old := range catalog {
			if old.Owner == c.Owner {
				owned++
			}
			if old.Name == c.Name {
				writeError(w, 409, "configuration name already in use")
				return
			}
		}
		if u.Role != "admin" && owned >= 32 {
			writeError(w, 403, "at most 32 configurations per user")
			return
		}
	}
	var ownerExists bool
	if err = a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM hub_users WHERE username=$1)`, c.Owner).Scan(&ownerExists); err != nil {
		writeError(w, 500, "could not check configuration owner")
		return
	}
	if !ownerExists {
		writeError(w, 400, "owner must be an existing account; bearer-token creation requires owner")
		return
	}
	if c.Global && u.Role != "admin" {
		writeError(w, 403, "only admins can map configurations to all workspaces")
		return
	}
	c.Mapping.Configuration = c.Name
	if input.Data != nil {
		c.Keys = []string{}
		size := 0
		for key, value := range input.Data {
			if len(key) > 253 || !configurationKeyPattern.MatchString(key) || key == "." || key == ".." {
				writeError(w, 400, "invalid configuration key")
				return
			}
			size += len(key) + len(value)
			c.Keys = append(c.Keys, key)
		}
		if len(c.Keys) == 0 || len(c.Keys) > 64 || size > 48000 {
			writeError(w, 400, "provide 1–64 keys, at most 48000 bytes total")
			return
		}
		slices.Sort(c.Keys)
	}
	if c.Global || len(c.Mapping.Files)+len(c.Mapping.Env) > 0 {
		if err = validateConfigurationMappings([]resolvedConfiguration{{c, c.Mapping}}); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	proposed := append([]configuration{}, catalog...)
	replaced := false
	for i := range proposed {
		if proposed[i].Name == c.Name {
			proposed[i] = c
			replaced = true
		}
	}
	if !replaced {
		proposed = append(proposed, c)
	}
	if err = a.validateConfigurationWorkspaces(r.Context(), proposed); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	version := make([]byte, 16)
	if _, err = rand.Read(version); err != nil {
		writeError(w, 500, "could not generate configuration revision")
		return
	}
	c.Version = hex.EncodeToString(version)
	// Reserve ownership before contacting Kubernetes. Failed saves remain visible
	// and unready; PUT with the same data repairs them without leaking values to DB.
	_, err = a.db.Exec(r.Context(), `INSERT INTO configurations(name,owner,kind,global,mapping,keys,version,ready) VALUES($1,$2,$3,$4,$5,$6,$7,false) ON CONFLICT(name) DO UPDATE SET global=$4,mapping=$5,keys=$6,version=$7,ready=false`, c.Name, c.Owner, c.Kind, c.Global, c.Mapping, c.Keys, c.Version)
	if err != nil {
		writeError(w, 500, "could not save configuration metadata")
		return
	}
	if err = a.kube.saveConfiguration(r.Context(), c, input.Data); err != nil {
		writeError(w, 502, "configuration reserved; retry with PUT and the same data: "+err.Error())
		return
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE configurations SET ready=true WHERE name=$1`, c.Name); err != nil {
		writeError(w, 500, "configuration saved; retry metadata update")
		return
	}
	if err = a.reconcileConfigurationsLocked(r.Context()); err != nil {
		writeError(w, 502, "configuration saved; workspace rollout needs retry: "+err.Error())
		return
	}
	status := 200
	if existing == nil {
		status = 201
	}
	writeJSON(w, status, c)
}

func (a *application) validateConfigurationWorkspaces(ctx context.Context, catalog []configuration) error {
	rows, err := a.db.Query(ctx, `SELECT name,owner FROM workspaces ORDER BY name`)
	if err != nil {
		return err
	}
	items := []workspace{}
	for rows.Next() {
		var item workspace
		if err = rows.Scan(&item.Name, &item.Owner); err != nil {
			break
		}
		items = append(items, item)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, item := range items {
		mappings, e := a.workspaceConfigurationMappings(ctx, item.Name)
		if e != nil {
			return e
		}
		if _, e = resolveConfigurationCatalog(catalog, item.Owner, mappings); e != nil {
			return fmt.Errorf("workspace %s: %w", item.Name, e)
		}
	}
	// Check global collisions even when no workspaces exist yet.
	_, err = resolveConfigurationCatalog(catalog, "", nil)
	return err
}
func (k *kubernetes) saveConfiguration(ctx context.Context, c configuration, data map[string]string) error {
	kind, name := configurationKind(c.Kind), configurationResource(c.Name)
	for attempt := 0; attempt < 5; attempt++ {
		obj, status, err := k.call(ctx, "GET", kind, name, nil)
		if status == 404 {
			if data == nil {
				return fmt.Errorf("resource missing; provide data to recreate it")
			}
			obj = object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": name, "namespace": k.namespace, "labels": object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/configuration": c.Name}}}
			if c.Kind == "secret" {
				obj["kind"] = "Secret"
				obj["type"] = "Opaque"
			}
		} else {
			if err != nil {
				return err
			}
			meta, _ := obj["metadata"].(object)
			labels, _ := meta["labels"].(object)
			if meta["deletionTimestamp"] != nil || labels["app.kubernetes.io/managed-by"] != "herdr-hub" || labels["herdr-hub/configuration"] != c.Name {
				return fmt.Errorf("configuration resource is deleting or unmanaged")
			}
		}
		if data != nil {
			values := object{}
			for key, value := range data {
				if c.Kind == "secret" {
					value = base64.StdEncoding.EncodeToString([]byte(value))
				}
				values[key] = value
			}
			obj["data"] = values
			delete(obj, "binaryData")
			delete(obj, "stringData")
		}
		values, _ := obj["data"].(object)
		for _, key := range c.Keys {
			if _, ok := values[key]; !ok {
				return fmt.Errorf("resource keys differ; retry with complete data")
			}
		}
		delete(obj, "status")
		method, target := "PUT", name
		if status == 404 {
			method, target = "POST", ""
		}
		_, status, err = k.call(ctx, method, kind, target, obj)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("configuration changed concurrently; retry")
}

func (a *application) mapConfigurations(w http.ResponseWriter, r *http.Request, name string) {
	var input struct {
		Configurations []configurationMapping `json:"configurations"`
	}
	if !decodeShared(w, r, &input) {
		return
	}
	if input.Configurations == nil {
		writeError(w, 400, "provide a configurations array")
		return
	}
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Status == "deleting" || item.Status == "undeploying" {
		writeError(w, 409, "finish workspace deletion or undeployment first")
		return
	}
	resolved, err := a.resolveConfigurations(r.Context(), item.Owner, input.Configurations)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// Persist desired state first; the worker retries failed runtime updates.
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `DELETE FROM workspace_configurations WHERE workspace=$1`, name)
	for _, m := range input.Configurations {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO workspace_configurations VALUES($1,$2,$3)`, name, m.Configuration, m)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "could not save mappings")
		return
	}
	if item.Status != "undeployed" {
		if err = a.kube.updateConfigurationMappings(r.Context(), name, resolved); err != nil {
			writeError(w, 502, "mappings saved; rollout needs retry: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, input)
}

func (a *application) deleteConfiguration(ctx context.Context, c configuration) error {
	var busy bool
	if err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_configurations WHERE configuration=$1)`, c.Name).Scan(&busy); err != nil {
		return err
	}
	if c.Global || busy {
		return fmt.Errorf("disable global mapping and detach from all workspaces before deleting")
	}
	// Failed rollouts/terminating pods may still use an already detached object.
	deployments, err := a.db.Query(ctx, `SELECT name FROM workspaces`)
	if err != nil {
		return err
	}
	names := []string{}
	for deployments.Next() {
		var name string
		if err = deployments.Scan(&name); err != nil {
			break
		}
		names = append(names, name)
	}
	deployments.Close()
	if err != nil {
		return err
	}
	if err = deployments.Err(); err != nil {
		return err
	}
	for _, name := range names {
		d, status, e := a.kube.call(ctx, "GET", "deployments", resourceName(name), nil)
		if status == 404 {
			continue
		}
		if e != nil {
			return e
		}
		spec, _ := d["spec"].(object)
		template, _ := spec["template"].(object)
		pod, _ := template["spec"].(object)
		if podUsesConfiguration(pod, configurationResource(c.Name)) {
			return fmt.Errorf("wait for workspace detach rollout before deleting")
		}
	}
	pods, err := a.kube.sharedPods(ctx)
	if err != nil {
		return err
	}
	for _, raw := range pods {
		pod, _ := raw.(object)
		spec, _ := pod["spec"].(object)
		if podUsesConfiguration(spec, configurationResource(c.Name)) {
			return fmt.Errorf("wait for old pods to terminate before deleting")
		}
	}
	resource, status, e := a.kube.call(ctx, "GET", configurationKind(c.Kind), configurationResource(c.Name), nil)
	if status != 404 {
		if e != nil {
			return e
		}
		meta, _ := resource["metadata"].(object)
		labels, _ := meta["labels"].(object)
		if labels["herdr-hub/configuration"] != c.Name {
			return fmt.Errorf("refusing to delete an unrelated configuration resource")
		}
	}
	if err = a.kube.remove(ctx, configurationKind(c.Kind), configurationResource(c.Name)); err != nil {
		return err
	}
	_, err = a.db.Exec(ctx, `DELETE FROM configurations WHERE name=$1`, c.Name)
	return err
}

func (a *application) reconcileConfigurationsLocked(ctx context.Context) error {
	rows, err := a.db.Query(ctx, `SELECT name FROM workspaces WHERE status NOT IN ('deleting','undeploying','undeployed') ORDER BY name`)
	if err != nil {
		return err
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		names = append(names, name)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	failures := []error{}
	for _, name := range names {
		if err = a.reconcileWorkspaceConfigurations(ctx, name); err != nil {
			failures = append(failures, fmt.Errorf("workspace %s: %w", name, err))
		}
	}
	return errors.Join(failures...)
}
func (a *application) reconcileWorkspaceConfigurations(ctx context.Context, name string) error {
	item, err := a.load(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.Status == "deleting" || item.Status == "undeploying" || item.Status == "undeployed" {
		return nil
	}
	resolved, err := a.resolveConfigurations(ctx, item.Owner, item.Configurations)
	if err != nil {
		return err
	}
	return a.kube.updateConfigurationMappings(ctx, name, resolved)
}

// Explicit file/env mappings reconcile independently of the image-only worker.
// Do not recreate missing or deleting Deployments or change unrelated pod fields.
func (a *application) runConfigurationUpdates(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		if err := a.mu.lock(passCtx); err == nil {
			if err = a.reconcileConfigurationsLocked(passCtx); err != nil {
				log.Printf("configuration rollout needs retry: %v", err)
			}
			a.mu.unlock()
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func configurationRevision(items []resolvedConfiguration) string {
	if len(items) == 0 {
		return ""
	}
	data, _ := json.Marshal(items)
	// Version is deliberately random, not a hash of secret values (which could
	// disclose low-entropy secrets through a visible Deployment annotation).
	versions := []string{}
	for _, item := range items {
		versions = append(versions, item.Configuration.Version)
	}
	data = append(data, strings.Join(versions, ",")...)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func objects(value any) []object {
	result := []object{}
	switch items := value.(type) {
	case []object:
		return items
	case []any:
		for _, raw := range items {
			if item, ok := raw.(object); ok {
				result = append(result, item)
			}
		}
	}
	return result
}
func managedConfigurationEnv(e object) bool {
	from, _ := e["valueFrom"].(object)
	for _, kind := range []string{"secretKeyRef", "configMapKeyRef"} {
		ref, _ := from[kind].(object)
		name, _ := ref["name"].(string)
		if strings.HasPrefix(name, "herdr-config-") {
			return true
		}
	}
	return false
}
func addConfigurationMappings(pod object, items []resolvedConfiguration) {
	volumes := []object{}
	for _, v := range objects(pod["volumes"]) {
		name, _ := v["name"].(string)
		if !strings.HasPrefix(name, "config-file-") {
			volumes = append(volumes, v)
		}
	}
	for _, c := range objects(pod["containers"]) {
		if c["name"] != "herdr" {
			continue
		}
		mounts, envs := []object{}, []object{}
		for _, m := range objects(c["volumeMounts"]) {
			name, _ := m["name"].(string)
			if !strings.HasPrefix(name, "config-file-") {
				mounts = append(mounts, m)
			}
		}
		for _, e := range objects(c["env"]) {
			if !managedConfigurationEnv(e) {
				envs = append(envs, e)
			}
		}
		index := 0
		for _, item := range items {
			resource := configurationResource(item.Configuration.Name)
			for _, f := range item.Mapping.Files {
				name := fmt.Sprintf("config-file-%d", index)
				index++
				v := object{"name": name}
				source := object{"items": []object{{"key": f.Key, "path": "value"}}, "defaultMode": 292}
				if item.Configuration.Kind == "secret" {
					source["secretName"] = resource
					source["defaultMode"] = 288
					v["secret"] = source
				} else {
					source["name"] = resource
					v["configMap"] = source
				}
				volumes = append(volumes, v)
				mounts = append(mounts, object{"name": name, "mountPath": f.Path, "subPath": "value", "readOnly": true})
			}
			for _, e := range item.Mapping.Env {
				kind := "configMapKeyRef"
				if item.Configuration.Kind == "secret" {
					kind = "secretKeyRef"
				}
				envs = append(envs, object{"name": e.Name, "valueFrom": object{kind: object{"name": resource, "key": e.Key}}})
			}
		}
		c["volumeMounts"], c["env"] = mounts, envs
	}
	pod["volumes"] = volumes
}
func (k *kubernetes) updateConfigurationMappings(ctx context.Context, name string, items []resolvedConfiguration) error {
	for attempt := 0; attempt < 5; attempt++ {
		d, status, err := k.call(ctx, "GET", "deployments", resourceName(name), nil)
		if status == 404 {
			return nil
		}
		if err != nil {
			return err
		}
		meta, _ := d["metadata"].(object)
		labels, _ := meta["labels"].(object)
		if labels["app.kubernetes.io/managed-by"] != "herdr-hub" {
			return fmt.Errorf("runtime is unmanaged")
		}
		if meta["deletionTimestamp"] != nil {
			return nil
		}
		spec, _ := d["spec"].(object)
		strategy, _ := spec["strategy"].(object)
		if strategy["type"] != "Recreate" {
			return fmt.Errorf("runtime must use Recreate")
		}
		template, _ := spec["template"].(object)
		pod, _ := template["spec"].(object)
		found := false
		for _, c := range objects(pod["containers"]) {
			if c["name"] != "herdr" {
				continue
			}
			found = true
			for _, e := range objects(c["env"]) {
				if managedConfigurationEnv(e) {
					continue
				}
				for _, item := range items {
					for _, mapping := range item.Mapping.Env {
						if e["name"] == mapping.Name {
							return fmt.Errorf("environment mapping conflicts with existing variable %s", mapping.Name)
						}
					}
				}
			}
			for _, m := range objects(c["volumeMounts"]) {
				mountName, _ := m["name"].(string)
				if strings.HasPrefix(mountName, "config-file-") || mountName == "home" {
					continue
				}
				mountPath, _ := m["mountPath"].(string)
				for _, item := range items {
					for _, f := range item.Mapping.Files {
						if mountPath == f.Path || strings.HasPrefix(f.Path, mountPath+"/") || strings.HasPrefix(mountPath, f.Path+"/") {
							return fmt.Errorf("configuration mapping conflicts with existing mount")
						}
					}
				}
			}
		}
		if !found {
			return fmt.Errorf("herdr container missing")
		}
		before, _ := json.Marshal(template)
		addConfigurationMappings(pod, items)
		templateMeta, _ := template["metadata"].(object)
		if templateMeta == nil {
			return fmt.Errorf("template metadata missing")
		}
		annotations, _ := templateMeta["annotations"].(object)
		revision := configurationRevision(items)
		if revision != "" {
			if annotations == nil {
				annotations = object{}
				templateMeta["annotations"] = annotations
			}
			annotations["herdr-hub/configuration-version"] = revision
		} else if annotations != nil {
			delete(annotations, "herdr-hub/configuration-version")
		}
		after, _ := json.Marshal(template)
		if string(before) == string(after) {
			return nil
		}
		delete(d, "status")
		_, status, err = k.call(ctx, "PUT", "deployments", resourceName(name), d)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("runtime changed concurrently; retry")
}
func podUsesConfiguration(pod object, name string) bool {
	for _, v := range objects(pod["volumes"]) {
		s, _ := v["secret"].(object)
		c, _ := v["configMap"].(object)
		if s["secretName"] == name || c["name"] == name {
			return true
		}
	}
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		for _, c := range objects(pod[field]) {
			for _, e := range objects(c["env"]) {
				from, _ := e["valueFrom"].(object)
				for _, kind := range []string{"secretKeyRef", "configMapKeyRef"} {
					ref, _ := from[kind].(object)
					if ref["name"] == name {
						return true
					}
				}
			}
			for _, e := range objects(c["envFrom"]) {
				for _, kind := range []string{"secretRef", "configMapRef"} {
					ref, _ := e[kind].(object)
					if ref["name"] == name {
						return true
					}
				}
			}
		}
	}
	return false
}
