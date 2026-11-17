package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type application struct {
	db              *pgxpool.Pool
	kube            *kubernetes
	webDist         string
	docsEnabled     bool
	token           string
	mu              contextMutex
	loginLimits     loginLimiter
	insecureCookies bool
	trustedProxies  []*net.IPNet
	auditLogger     *log.Logger
}

type workspace struct {
	Usage                  *workspaceUsage         `json:"usage"`
	Resources              workspaceResources      `json:"resources"`
	Owner                  string                  `json:"owner"`
	StorageSize            string                  `json:"storage_size"`
	Name                   string                  `json:"name"`
	SSHKey                 string                  `json:"ssh_public_key,omitempty"`
	SSHListener            string                  `json:"ssh_listener,omitempty"`
	SSHPort                int                     `json:"ssh_port,omitempty"`
	Status                 string                  `json:"status"`
	Error                  string                  `json:"error,omitempty"`
	URL                    string                  `json:"url"`
	SSHHost                string                  `json:"ssh_host,omitempty"`
	CreatedAt              time.Time               `json:"created_at"`
	Volumes                []string                `json:"volumes"`
	Configurations         []configurationMapping  `json:"configurations"`
	Ports                  []workspacePort         `json:"ports"`
	Egress                 egressSettings          `json:"egress"`
	ResolvedConfigurations []resolvedConfiguration `json:"-"`
}

func main() {
	token := os.Getenv("HUB_API_TOKEN")
	if len(token) < 32 {
		log.Fatal("HUB_API_TOKEN must contain at least 32 bytes; use a cryptographically random token")
	}
	kube, err := newKubernetes()
	if err != nil {
		log.Fatal(err)
	}
	u := &url.URL{Scheme: "postgres", User: url.UserPassword(env("DB_USER", "herdr"), os.Getenv("DB_PASSWORD")), Host: net.JoinHostPort(env("DB_HOST", "localhost"), env("DB_PORT", "5432")), Path: "/" + env("DB_NAME", "herdr")}
	q := url.Values{}
	q.Set("sslmode", env("DB_SSLMODE", "require"))
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	err = initializeDatabase(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	a := &application{db: db, kube: kube, webDist: env("WEB_DIST_DIR", "web/dist"), token: token}
	a.docsEnabled = env("HUB_DOCS_ENABLED", "true") == "true"
	a.insecureCookies = os.Getenv("HUB_INSECURE_COOKIES") == "true"
	for _, cidr := range strings.Split(os.Getenv("HUB_TRUSTED_PROXY_CIDRS"), ",") {
		if strings.TrimSpace(cidr) == "" {
			continue
		}
		_, network, e := net.ParseCIDR(strings.TrimSpace(cidr))
		if e != nil {
			log.Fatal("invalid HUB_TRUSTED_PROXY_CIDRS")
		}
		a.trustedProxies = append(a.trustedProxies, network)
	}
	if err = a.bootstrapAdmin(ctx, os.Getenv("HUB_ADMIN_USERNAME"), os.Getenv("HUB_ADMIN_PASSWORD")); err != nil {
		log.Fatal(err)
	}
	if err = a.upgradeSSH(ctx); err != nil {
		log.Printf("legacy SSH upgrade needs retry: %v", err)
	}
	go a.runWorkspaceImageUpdates(context.Background())
	go a.runConfigurationUpdates(context.Background())
	go a.runPortUpdates(context.Background())
	go a.runEgressUpdates(context.Background())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, c := context.WithTimeout(r.Context(), 2*time.Second)
		defer c()
		if db.Ping(ctx) != nil {
			writeError(w, 503, "database unavailable")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/", a.api)
	mux.HandleFunc("/", a.serveWeb)
	log.Print("Herdr Hub Kubernetes provisioner listening on :8080")
	log.Fatal(newHTTPServer(":"+env("PORT", "8080"), mux).ListenAndServe())
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
}

func initializeDatabase(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS workspaces (
 name TEXT PRIMARY KEY, ssh_public_key TEXT NOT NULL, ssh_listener TEXT NOT NULL DEFAULT '',
 ssh_port INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'pending',
 error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now());
 CREATE UNIQUE INDEX IF NOT EXISTS workspace_ssh_listener ON workspaces(ssh_listener) WHERE ssh_listener <> '';
 CREATE TABLE IF NOT EXISTS hub_users(username TEXT PRIMARY KEY,password_hash TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('admin','user')));
  CREATE TABLE IF NOT EXISTS hub_sessions(hash TEXT PRIMARY KEY,username TEXT NOT NULL REFERENCES hub_users(username) ON DELETE CASCADE,expires_at TIMESTAMPTZ NOT NULL);
  CREATE TABLE IF NOT EXISTS hub_api_tokens(id TEXT PRIMARY KEY,hash TEXT UNIQUE NOT NULL,name TEXT NOT NULL,username TEXT NOT NULL REFERENCES hub_users(username) ON DELETE CASCADE,created_at TIMESTAMPTZ NOT NULL DEFAULT now());
 ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owner TEXT NOT NULL DEFAULT '';
 ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS storage_size TEXT NOT NULL DEFAULT '';
  ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS volumes JSONB NOT NULL DEFAULT '[]';
  ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS resources JSONB NOT NULL DEFAULT '{"cpu_request":"500m","cpu_limit":"","memory_request":"1Gi","memory_limit":"4Gi"}';
  CREATE TABLE IF NOT EXISTS hub_resource_defaults(id INTEGER PRIMARY KEY CHECK(id=1), resources JSONB NOT NULL);
  INSERT INTO hub_resource_defaults(id,resources) VALUES(1,'{"cpu_request":"500m","cpu_limit":"2","memory_request":"1Gi","memory_limit":"4Gi"}') ON CONFLICT DO NOTHING;
  ALTER TABLE hub_resource_defaults ADD COLUMN IF NOT EXISTS security_limits JSONB NOT NULL DEFAULT '{"cpu":"4","memory":"8Gi","volume_gi":100,"user_storage_gi":200,"user_volumes":10}';
  CREATE TABLE IF NOT EXISTS hub_groups(name TEXT PRIMARY KEY, owner TEXT NOT NULL REFERENCES hub_users(username));
 CREATE TABLE IF NOT EXISTS hub_group_members(group_name TEXT REFERENCES hub_groups(name) ON DELETE CASCADE, username TEXT REFERENCES hub_users(username) ON DELETE CASCADE, PRIMARY KEY(group_name,username));
 CREATE TABLE IF NOT EXISTS shared_volumes(name TEXT PRIMARY KEY, owner TEXT NOT NULL REFERENCES hub_users(username), storage_size TEXT NOT NULL, storage_class TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS volume_users(volume TEXT REFERENCES shared_volumes(name) ON DELETE CASCADE, username TEXT REFERENCES hub_users(username) ON DELETE CASCADE, PRIMARY KEY(volume,username));
  CREATE TABLE IF NOT EXISTS volume_groups(volume TEXT REFERENCES shared_volumes(name) ON DELETE CASCADE, group_name TEXT REFERENCES hub_groups(name) ON DELETE CASCADE, PRIMARY KEY(volume,group_name));
  CREATE TABLE IF NOT EXISTS configurations(name TEXT PRIMARY KEY, owner TEXT NOT NULL REFERENCES hub_users(username), kind TEXT NOT NULL CHECK(kind IN ('configmap','secret')), global BOOLEAN NOT NULL DEFAULT false, mapping JSONB NOT NULL, keys JSONB NOT NULL, version TEXT NOT NULL, ready BOOLEAN NOT NULL DEFAULT false);
   CREATE TABLE IF NOT EXISTS workspace_configurations(workspace TEXT REFERENCES workspaces(name) ON DELETE CASCADE, configuration TEXT REFERENCES configurations(name), mapping JSONB NOT NULL, PRIMARY KEY(workspace,configuration));
   CREATE TABLE IF NOT EXISTS workspace_ports(workspace TEXT NOT NULL REFERENCES workspaces(name) ON DELETE CASCADE, name TEXT NOT NULL, id TEXT UNIQUE NOT NULL, protocol TEXT NOT NULL CHECK(protocol IN ('HTTP','TCP','UDP')), internal_port INTEGER NOT NULL CHECK(internal_port BETWEEN 1 AND 65535), external_port INTEGER NOT NULL DEFAULT 0, deleting BOOLEAN NOT NULL DEFAULT false, status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,name));
    CREATE UNIQUE INDEX IF NOT EXISTS workspace_external_port ON workspace_ports(external_port) WHERE external_port>0;
    CREATE TABLE IF NOT EXISTS hub_egress_defaults(id INTEGER PRIMARY KEY CHECK(id=1), rules JSONB NOT NULL, status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '');
    CREATE TABLE IF NOT EXISTS workspace_egress(workspace TEXT PRIMARY KEY REFERENCES workspaces(name) ON DELETE CASCADE, rules JSONB NOT NULL, status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '');`)
	return err
}

func (a *application) api(w http.ResponseWriter, r *http.Request) {
	a.browserHeaders(w)
	recorder := &auditResponse{ResponseWriter: w}
	w = recorder
	var u identity
	loginHandled := false
	defer func() {
		if !loginHandled && (r.Method != "GET" || recorder.status >= 400 || strings.HasSuffix(r.URL.Path, "/token")) {
			a.audit(r, u, recorder.status)
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "GET" && r.Header.Get("Origin") != "" {
		origin, err := url.Parse(r.Header.Get("Origin"))
		expectedScheme := "https"
		if a.insecureCookies {
			expectedScheme = "http"
		}
		if err != nil || origin.Host != r.Host || origin.Scheme != expectedScheme || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			writeError(w, 403, "invalid request origin")
			return
		}
	}
	if r.URL.Path == "/api/login" && r.Method == "POST" {
		if !a.loginLimits.allowIP(a.clientIP(r), time.Now()) {
			loginThrottled(w)
			return
		}
		if !bufferRequestBody(w, r, 4096) {
			return
		}
		loginHandled = true
		a.login(w, r)
		return
	}
	var authenticated bool
	u, authenticated = a.authenticate(r)
	if !authenticated {
		writeError(w, 401, "Hub access token required")
		return
	}
	// Serialize mutations in this single-replica provisioner. Database uniqueness
	// also prevents two workspaces from claiming the same SSH listener.
	if r.Method != "GET" || strings.HasSuffix(r.URL.Path, "/token") {
		if !bufferRequestBody(w, r, 65536) {
			return
		}
	}
	// Sensitive token reads also serialize with transfers to avoid an old owner
	// obtaining the newly rotated credential after a concurrent reassignment.
	if r.Method != "GET" || strings.HasSuffix(r.URL.Path, "/token") {
		if err := a.mu.lock(ctx); err != nil {
			writeError(w, 503, "request expired waiting for another operation")
			return
		}
		defer a.mu.unlock()
		// An account can be revoked/demoted while this request waits for the lock.
		u, authenticated = a.authenticate(r)
		if !authenticated {
			writeError(w, 401, "session expired")
			return
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, u))
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	if p == "me" && r.Method == "GET" {
		writeJSON(w, 200, u)
		return
	}
	if p == "logout" && r.Method == "POST" {
		if c, err := r.Cookie(a.sessionCookieName()); err == nil {
			if _, err = a.db.Exec(ctx, `DELETE FROM hub_sessions WHERE hash=$1`, sessionHash(c.Value)); err != nil {
				writeError(w, 500, "logout failed")
				return
			}
		}
		http.SetCookie(w, a.sessionCookie("", -1))
		w.WriteHeader(204)
		return
	}
	parts := strings.Split(p, "/")
	if p == "egress-defaults" {
		a.egressAPI(w, r, "")
		return
	}
	if len(parts) == 3 && parts[0] == "workspaces" && validName(parts[1]) && parts[2] == "egress" {
		a.egressAPI(w, r, parts[1])
		return
	}
	if len(parts) >= 3 && parts[0] == "workspaces" && validName(parts[1]) && parts[2] == "ports" {
		a.portsAPI(w, r, parts)
		return
	}
	if parts[0] == "api-tokens" {
		a.apiTokens(w, r, parts)
		return
	}
	if parts[0] == "configurations" {
		a.configurationsAPI(w, r, parts)
		return
	}
	if len(parts) == 2 && parts[0] == "directory" && r.Method == "GET" {
		a.searchDirectory(w, r, parts[1])
		return
	}
	if parts[0] == "volumes" || parts[0] == "groups" {
		a.sharedAPI(w, r, parts)
		return
	}
	keyUpdate := len(parts) == 3 && parts[0] == "workspaces" && validName(parts[1]) && parts[2] == "ssh-key" && r.Method == "PATCH"
	mountUpdate := len(parts) == 3 && parts[0] == "workspaces" && validName(parts[1]) && parts[2] == "volumes" && r.Method == "PUT"
	lifecycleUpdate := len(parts) == 3 && parts[0] == "workspaces" && validName(parts[1]) && (parts[2] == "deploy" || parts[2] == "undeploy") && r.Method == "POST"
	resourceUpdate := len(parts) == 3 && parts[0] == "workspaces" && validName(parts[1]) && parts[2] == "resources" && r.Method == "PATCH"
	configurationUpdate := len(parts) == 3 && parts[0] == "workspaces" && validName(parts[1]) && parts[2] == "configurations" && r.Method == "PUT"
	if (parts[0] == "users" || (r.Method != "GET" && r.Method != "DELETE" && !keyUpdate && !mountUpdate && !lifecycleUpdate && !resourceUpdate && !configurationUpdate)) && u.Role != "admin" {
		writeError(w, 403, "admin role required")
		return
	}
	if p == "users" && (r.Method == "GET" || r.Method == "POST") {
		a.users(w, r)
		return
	}
	if len(parts) == 2 && parts[0] == "users" && validAccountName(parts[1]) && (r.Method == "PATCH" || r.Method == "DELETE") {
		a.manageUser(w, r, parts[1])
		return
	}
	if p == "config" && (r.Method == "GET" || r.Method == "PATCH") {
		a.config(w, r)
		return
	}
	if p == "workspaces" {
		switch r.Method {
		case "GET":
			a.list(w, r)
			return
		case "POST":
			a.create(w, r)
			return
		}
	}
	if len(parts) >= 2 && parts[0] == "workspaces" && validName(parts[1]) {
		if len(parts) == 3 && parts[2] == "owner" && r.Method == "PATCH" {
			a.assignOwner(w, r, parts[1])
			return
		}
		if u.Role != "admin" {
			item, err := a.load(ctx, parts[1])
			if err != nil || item.Owner != u.Username {
				writeError(w, 404, "workspace not found")
				return
			}
		}
		if len(parts) == 3 && parts[2] == "size" && r.Method == "PATCH" {
			a.resize(w, r, parts[1])
			return
		}
		if resourceUpdate {
			a.updateWorkspaceResources(w, r, parts[1])
			return
		}
		if keyUpdate {
			a.updateSSHKey(w, r, parts[1])
			return
		}
		if mountUpdate {
			a.mountVolumes(w, r, parts[1])
			return
		}
		if configurationUpdate {
			a.mapConfigurations(w, r, parts[1])
			return
		}
		if lifecycleUpdate {
			if parts[2] == "undeploy" {
				a.undeploy(w, r, parts[1])
			} else {
				a.provision(w, r, parts[1])
			}
			return
		}
		if len(parts) == 2 && r.Method == "DELETE" {
			a.remove(w, r, parts[1])
			return
		}
		if len(parts) == 3 && parts[2] == "retry" && r.Method == "POST" {
			a.provision(w, r, parts[1])
			return
		}
		if len(parts) == 3 && parts[2] == "token" && r.Method == "GET" {
			var exists bool
			if err := a.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM workspaces WHERE name=$1)", parts[1]).Scan(&exists); err != nil || !exists {
				writeError(w, 404, "workspace not found")
				return
			}
			secret, err := a.kube.get(ctx, "secrets", resourceName(parts[1]))
			if err != nil {
				writeError(w, 502, "workspace token unavailable")
				return
			}
			data, ok := secret["data"].(map[string]any)
			token, tokenOK := data["token"].(string)
			if !ok || !tokenOK || token == "" {
				writeError(w, 502, "workspace token missing from Secret")
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, 200, map[string]any{"token_base64": token})
			return
		}
		if len(parts) == 3 && parts[2] == "token" && r.Method == "POST" {
			a.rotateWorkspaceToken(w, r, parts[1])
			return
		}
	}
	writeError(w, 404, "endpoint not found")
}

func (a *application) create(w http.ResponseWriter, r *http.Request) {
	var item workspace
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&item); err != nil {
		writeError(w, 400, "invalid workspace JSON")
		return
	}
	if !validName(item.Name) {
		writeError(w, 400, "name must be a lowercase DNS label, at most 40 characters")
		return
	}
	if strings.HasPrefix(item.Name, "shared-") || strings.HasPrefix(item.Name, "config-") || strings.HasPrefix(item.Name, "export-") {
		writeError(w, 400, "workspace names starting with shared-, config- or export- are reserved")
		return
	}
	keys, keyErr := normalizePublicKeys(item.SSHKey)
	if keyErr != nil || keys == "" {
		writeError(w, 400, "provide at least one valid OpenSSH public key, not a private key")
		return
	}
	item.SSHKey = keys
	item.SSHPort = 0
	if len(item.Volumes) != 0 {
		writeError(w, 400, "attach shared volumes after creating the workspace")
		return
	}
	if item.Owner == "" {
		item.Owner = currentUser(r).Username
	}
	if item.Owner != "" {
		var exists bool
		if err := a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM hub_users WHERE username=$1)`, item.Owner).Scan(&exists); err != nil || !exists {
			writeError(w, 400, "owner must be an existing user")
			return
		}
	}
	// Validate private attachments and global collisions before saving anything
	// or reserving runtime resources. Admins still act for the workspace owner.
	if _, err := a.resolveConfigurations(r.Context(), item.Owner, item.Configurations); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if item.StorageSize == "" {
		item.StorageSize = a.kube.storageSize
	}
	if _, err := storageGi(item.StorageSize); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// Reattached homes keep their actual capacity. Expansion is a separate,
	// explicit operation rather than changing the create-only retry behavior.
	if pvc, status, e := a.kube.call(r.Context(), "GET", "persistentvolumeclaims", resourceName(item.Name), nil); status != 404 {
		if e != nil {
			writeError(w, 502, "could not inspect retained home")
			return
		}
		meta, _ := pvc["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if labels["app.kubernetes.io/managed-by"] != "herdr-hub" {
			writeError(w, 409, "retained home is not owned by Herdr Hub")
			return
		}
		spec, _ := pvc["spec"].(map[string]any)
		resources, _ := spec["resources"].(map[string]any)
		requests, _ := resources["requests"].(map[string]any)
		actual, _ := requests["storage"].(string)
		if _, e = storageGi(actual); e != nil {
			writeError(w, 409, "retained home size is unsupported")
			return
		}
		item.StorageSize = actual
	}
	port, err := a.allocateSSH(r.Context())
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	item.SSHPort = port
	item.SSHListener = resourceName(item.Name)
	if item.Resources == (workspaceResources{}) {
		item.Resources, err = a.resourceDefaults(r.Context())
		if err != nil {
			writeError(w, 500, "could not load resource defaults")
			return
		}
	}
	if err = item.Resources.validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// Workspace and initial mappings must be atomic. Provisioning starts only
	// after commit, so even a failed first rollout retains attachments for retry.
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), "INSERT INTO workspaces(name,ssh_public_key,ssh_listener,ssh_port,owner,storage_size,resources) VALUES($1,$2,$3,$4,$5,$6,$7)", item.Name, item.SSHKey, item.SSHListener, item.SSHPort, item.Owner, item.StorageSize, item.Resources)
	if err != nil {
		writeError(w, 409, "workspace name or SSH listener is already in use, or database insert failed")
		return
	}
	for _, mapping := range item.Configurations {
		if _, err = tx.Exec(r.Context(), `INSERT INTO workspace_configurations(workspace,configuration,mapping) VALUES($1,$2,$3)`, item.Name, mapping.Configuration, mapping); err != nil {
			writeError(w, 500, "could not save initial configuration mappings")
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "could not save workspace and configuration mappings")
		return
	}
	a.provision(w, r, item.Name)
}

func (a *application) load(ctx context.Context, name string) (workspace, error) {
	var x workspace
	err := a.db.QueryRow(ctx, "SELECT name,ssh_public_key,ssh_listener,ssh_port,status,error,created_at,owner,storage_size,volumes,resources FROM workspaces WHERE name=$1", name).Scan(&x.Name, &x.SSHKey, &x.SSHListener, &x.SSHPort, &x.Status, &x.Error, &x.CreatedAt, &x.Owner, &x.StorageSize, &x.Volumes, &x.Resources)
	if x.StorageSize == "" {
		x.StorageSize = a.kube.storageSize
	}
	x.URL = "https://" + name + "." + a.kube.domain
	x.SSHHost = a.kube.sshHost
	if err == nil {
		x.Configurations, err = a.workspaceConfigurationMappings(ctx, name)
	}
	if err == nil {
		x.Ports, err = a.workspacePorts(ctx, name)
	}
	if err == nil {
		x.Egress, err = a.egressSettings(ctx, name)
	}
	return x, err
}

func (a *application) provision(w http.ResponseWriter, r *http.Request, name string) {
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Status == "deleting" || item.Status == "undeploying" {
		writeError(w, 409, "finish deleting or undeploying this workspace first")
		return
	}
	if item.SSHPort == 0 {
		item.SSHPort, err = a.allocateSSH(r.Context())
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		item.SSHListener = resourceName(name)
		if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET ssh_port=$2,ssh_listener=$3 WHERE name=$1`, name, item.SSHPort, item.SSHListener); err != nil {
			writeError(w, 500, "could not save SSH allocation")
			return
		}
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		writeError(w, 500, "could not generate workspace token")
		return
	}
	// Secret is create-only: retries preserve the original web token.
	item.ResolvedConfigurations, err = a.resolveConfigurations(r.Context(), item.Owner, item.Configurations)
	if err == nil {
		err = a.kube.workspaceListener(r.Context(), item, false)
	}
	for _, obj := range a.kube.resources(item, hex.EncodeToString(random)) {
		if err != nil {
			break
		}
		if err = a.kube.ensure(r.Context(), obj); err != nil {
			break
		}
	}
	if err == nil {
		err = a.syncWorkspacePorts(r.Context(), name, false)
	}
	if err == nil {
		err = a.syncEgress(r.Context(), name)
	}
	status, message := "provisioning", ""
	if err != nil {
		status = "error"
		message = err.Error()
	}
	_, dbErr := a.db.Exec(r.Context(), "UPDATE workspaces SET status=$2,error=$3 WHERE name=$1", name, status, message)
	if dbErr != nil {
		writeError(w, 500, "resources may exist; database status update failed")
		return
	}
	item.Status = status
	item.Error = message
	if err != nil {
		writeJSON(w, 502, item)
		return
	}
	writeJSON(w, 201, item)
}

func (a *application) list(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := a.db.Query(r.Context(), "SELECT name FROM workspaces WHERE $1='admin' OR owner=$2 ORDER BY created_at DESC", u.Role, u.Username)
	if err != nil {
		writeError(w, 500, "could not list workspaces")
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			break
		}
		names = append(names, n)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		writeError(w, 500, "could not read workspaces")
		return
	}
	result := []workspace{}
	for _, name := range names {
		item, e := a.load(r.Context(), name)
		if e != nil {
			writeError(w, 500, "could not load workspace")
			return
		}
		if u.Role != "admin" && item.Owner != u.Username {
			continue
		}
		if item.Status != "error" && item.Status != "deleting" && item.Status != "undeployed" && item.Status != "undeploying" {
			obj, e := a.kube.get(r.Context(), "deployments", resourceName(name))
			item.Status = "provisioning"
			if e != nil {
				item.Status = "unavailable"
				item.Error = "Kubernetes deployment unavailable"
			} else if s, ok := obj["status"].(map[string]any); ok {
				if ready, _ := s["readyReplicas"].(float64); ready > 0 {
					item.Status = "running"
				}
			}
		}
		result = append(result, item)
	}
	active := []string{}
	for _, item := range result {
		if item.Status != "undeployed" {
			active = append(active, item.Name)
		}
	}
	if len(active) > 0 {
		usage := a.kube.workspaceUsage(r.Context(), active)
		for i := range result {
			result[i].Usage = usage[result[i].Name]
		}
	}
	writeJSON(w, 200, result)
}

func (a *application) remove(w http.ResponseWriter, r *http.Request, name string) {
	item, loadErr := a.load(r.Context(), name)
	if loadErr != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if _, err := a.load(r.Context(), name); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "workspace not found")
		return
	} else if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	if _, err := a.db.Exec(r.Context(), "UPDATE workspaces SET status='deleting' WHERE name=$1", name); err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	// Retain the home PVC so deleting a runtime never silently erases user files.
	if err := a.kube.saveEgressPolicy(r.Context(), name, nil); err != nil {
		writeError(w, 502, "egress cleanup incomplete; retry: "+err.Error())
		return
	}
	if err := a.syncWorkspacePorts(r.Context(), name, true); err != nil {
		writeError(w, 502, "port cleanup incomplete; retry: "+err.Error())
		return
	}
	for _, kind := range []string{"httproutes", "tcproutes", "deployments", "services", "secrets"} {
		if err := a.kube.remove(r.Context(), kind, resourceName(name)); err != nil {
			writeError(w, 502, "deletion incomplete; retry: "+err.Error())
			return
		}
	}
	for {
		_, status, err := a.kube.call(r.Context(), "GET", "deployments", resourceName(name), nil)
		if status == 404 {
			break
		}
		if err != nil {
			writeError(w, 502, "waiting for runtime deletion; retry deletion")
			return
		}
		select {
		case <-r.Context().Done():
			writeError(w, 504, "runtime still deleting; retry deletion")
			return
		case <-time.After(time.Second):
		}
	}
	if err := a.kube.workspaceListener(r.Context(), item, true); err != nil {
		writeError(w, 502, "listener cleanup failed; retry: "+err.Error())
		return
	}
	if _, err := a.db.Exec(r.Context(), "DELETE FROM workspaces WHERE name=$1", name); err != nil {
		writeError(w, 500, "database cleanup failed; retry deletion")
		return
	}
	w.WriteHeader(204)
}

func (a *application) serveWeb(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/docs" || strings.HasPrefix(r.URL.Path, "/docs/") {
		a.serveDocs(w, r)
		return
	}
	a.browserHeaders(w)
	if r.Method != "GET" && r.Method != "HEAD" {
		writeError(w, 405, "method not allowed")
		return
	}
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, 404, "endpoint not found")
		return
	}
	file := filepath.Join(a.webDist, filepath.Clean("/"+r.URL.Path))
	if info, err := os.Stat(file); err != nil || info.IsDir() {
		file = filepath.Join(a.webDist, "index.html")
	}
	http.ServeFile(w, r, file)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, s string) {
	writeJSON(w, status, map[string]string{"error": s})
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
