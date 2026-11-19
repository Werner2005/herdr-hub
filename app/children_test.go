package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Werner2005/herdr-hub/internal/hubclient"
	"github.com/Werner2005/herdr-hub/internal/workload"
	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

type childFakeKube struct {
	mu        sync.Mutex
	stored    map[string]object
	delayPods bool
	execArgs  []string
}

func (f *childFakeKube) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/exec") {
		f.execArgs = r.URL.Query()["command"]
		if r.URL.Query().Get("stdin") != "false" || r.URL.Query().Get("container") != "workload" {
			http.Error(w, "bad exec options", 400)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v4.channel.k8s.io"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, frame := range [][]byte{
			append([]byte{1}, []byte("output\n")...), append([]byte{2}, []byte("warning\n")...),
			append([]byte{3}, []byte(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"7"}]}}`)...),
		} {
			if conn.Write(r.Context(), websocket.MessageBinary, frame) != nil {
				return
			}
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/log") {
		_, _ = fmt.Fprint(w, "hello from workload\n")
		return
	}
	switch r.Method {
	case "GET":
		obj := f.stored[r.URL.Path]
		if obj == nil {
			writeError(w, 404, "not found")
			return
		}
		writeJSON(w, 200, obj)
	case "POST":
		var obj object
		if json.NewDecoder(r.Body).Decode(&obj) != nil {
			writeError(w, 400, "invalid object")
			return
		}
		meta := obj["metadata"].(object)
		name := meta["name"].(string)
		meta["uid"] = "uid-" + name
		key := r.URL.Path + "/" + name
		if f.stored[key] != nil {
			writeError(w, 409, "already exists")
			return
		}
		if obj["kind"] == "Pod" {
			obj["status"] = object{"phase": "Running"}
		}
		f.stored[key] = obj
		writeJSON(w, 201, obj)
	case "DELETE":
		obj := f.stored[r.URL.Path]
		if obj == nil {
			writeError(w, 404, "not found")
			return
		}
		var options struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&options)
		if options.Preconditions.UID != obj["metadata"].(object)["uid"] {
			writeError(w, 409, "wrong deletion UID")
			return
		}
		if f.delayPods && obj["kind"] == "Pod" {
			obj["metadata"].(object)["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
		} else {
			delete(f.stored, r.URL.Path)
		}
		writeJSON(w, 200, object{"status": "Success"})
	default:
		writeError(w, 405, "unsupported fake request")
	}
}

func TestChildPodIsolation(t *testing.T) {
	k := &kubernetes{namespace: "test"}
	c := childRecord{Item: workload.Item{ID: "abc", Name: "database", Spec: workload.Spec{
		Image: "postgres:17", CPU: "500m", Memory: "512Mi", Entrypoint: []string{"sh"}, Command: []string{"-c", "echo 'a b'"},
		Ports: []int{5432},
	}}, Parent: "parent", Mounts: []childMount{{Home: true, SubPath: "workspace/project", Target: "/app", ReadOnly: true}}}
	pod := k.childPod(c)
	labels := pod["metadata"].(object)["labels"].(object)
	if labels["herdr-hub/workspace"] != nil || labels["herdr-hub/child-parent"] != c.Parent {
		t.Fatal("child can be selected by a parent Service or ReplicaSet")
	}
	spec := pod["spec"].(object)
	if spec["automountServiceAccountToken"] != false || spec["restartPolicy"] != "Never" || spec["enableServiceLinks"] != false {
		t.Fatal("unsafe child pod defaults")
	}
	container := spec["containers"].([]object)[0]
	security := container["securityContext"].(object)
	if security["runAsUser"] != 1001 || security["runAsNonRoot"] != true || security["allowPrivilegeEscalation"] != false {
		t.Fatal("child privilege boundary changed")
	}
	if !reflect.DeepEqual(container["args"], c.Spec.Command) || !reflect.DeepEqual(container["command"], c.Spec.Entrypoint) {
		t.Fatal("shell or entrypoint semantics changed")
	}
	if container["env"] != nil || len(container["envFrom"].([]object)) != 1 {
		t.Fatal("environment must be supplied through its own Secret")
	}
	mount := container["volumeMounts"].([]object)[0]
	if mount["subPath"] != "workspace/project" || mount["readOnly"] != true {
		t.Fatal("directory confinement or readonly mount lost")
	}
	policies := k.childNetwork(c)
	if len(policies) != 3 || policies[0]["spec"].(object)["podSelector"].(object)["matchLabels"].(object)["herdr-hub/child-id"] != c.ID {
		t.Fatal("child networking scope lost")
	}
}

func TestChildFeatureDisabledBeforeDatabase(t *testing.T) {
	t.Setenv("WORKLOADS_ENABLED", "false")
	a := &application{}
	w := httptest.NewRecorder()
	a.api(w, httptest.NewRequest("POST", workload.APIPrefix+"/workloads", strings.NewReader(`{}`)))
	if w.Code != 409 {
		t.Fatalf("disabled workload API: %d", w.Code)
	}
}

func TestChildWorkloadLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; dedicated test database required")
	}
	t.Setenv("WORKLOADS_ENABLED", "true")
	t.Setenv("WORKSPACE_NETWORK_POLICY_ENABLED", "true")
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initializeDatabase(ctx, db); err != nil {
		t.Fatal(err)
	}
	id, _ := childID()
	parent, other := "child-test-"+id[:12], "child-other-"+id[:12]
	for _, name := range []string{parent, other} {
		if _, err := db.Exec(ctx, `INSERT INTO hub_users(username,password_hash,role) VALUES($1,'unused','user');
`, name); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO workspaces(name,owner,ssh_public_key,status) VALUES($1,$1,'','ready')`, name); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = db.Exec(ctx, `DELETE FROM hub_children WHERE parent=ANY($1)`, []string{parent, other})
		_, _ = db.Exec(ctx, `DELETE FROM hub_child_volumes WHERE parent=ANY($1)`, []string{parent, other})
		_, _ = db.Exec(ctx, `DELETE FROM workspaces WHERE name=ANY($1)`, []string{parent, other})
		_, _ = db.Exec(ctx, `DELETE FROM hub_users WHERE username=ANY($1)`, []string{parent, other})
	}()
	fake := &childFakeKube{stored: map[string]object{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "kube-token")
	if err := os.WriteFile(tokenFile, []byte("test-only-kubernetes-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	k := &kubernetes{base: server.URL, namespace: "test", tokenFile: tokenFile, client: server.Client(), storageClass: "test-rwx"}
	a := &application{db: db, kube: k, token: "test-bootstrap"}
	request := func(method, path, body, token string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.api(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	control := func(parent, action string) string { return "/api/workspaces/" + parent + "/" + action }
	budget := `{"enabled":true,"max_workloads":2,"cpu":"1","memory":"1Gi","storage_gi":3,"max_volumes":3}`
	for _, name := range []string{parent, other} {
		request("PUT", control(name, "workload-access"), budget, a.token, 200)
	}
	session := strings.Repeat("a", 64)
	if _, err := db.Exec(ctx, `INSERT INTO hub_sessions(hash,username,expires_at) VALUES($1,$2,now()+interval '1 hour')`, sessionHash(session), parent); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", control(parent, "workload-access"), "", 200},
		{"PUT", control(parent, "workload-access"), budget, 403},
		{"GET", control(other, "workload-access"), "", 404},
		{"POST", control(other, "workload-token"), "", 404},
	} {
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		r.AddCookie(a.sessionCookie(session, 3600))
		w := httptest.NewRecorder()
		a.api(w, r)
		if w.Code != test.status {
			t.Fatalf("owner control authorization: %s %s: %d want %d", test.method, test.path, w.Code, test.status)
		}
	}
	mint := func(parent string) string {
		t.Helper()
		w := request("POST", control(parent, "workload-token"), "", a.token, 201)
		var result struct{ Token string }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !validChildToken(result.Token) {
			t.Fatal("invalid scoped credential", err)
		}
		var hash string
		if err := db.QueryRow(ctx, `SELECT hash FROM hub_child_tokens WHERE parent=$1`, parent).Scan(&hash); err != nil || hash != sessionHash(result.Token) {
			t.Fatal("credential was not stored hashed")
		}
		return result.Token
	}
	token, otherToken := mint(parent), mint(other)
	request("GET", "/api/me", "", token, 401)
	request("GET", workload.APIPrefix+"/workloads", "", a.token, 401)
	request("GET", workload.APIPrefix+"/workloads", "", otherToken, 200)
	request("POST", workload.APIPrefix+"/workloads", `{"owner":"forged"}`, token, 400)
	request("POST", workload.APIPrefix+"/workloads", `{} {}`, token, 400)
	request("POST", workload.APIPrefix+"/volumes", `{"name":"data","size":"1Gi"}`, token, 201)
	spec := workload.Spec{Name: "database", Image: "postgres:17", CPU: "500m", Memory: "512Mi", Env: map[string]string{"PASSWORD": "test-only-env-value"}, Ports: []int{5432},
		Mounts: []workload.Mount{{Kind: "named", Source: "data", Target: "/data"}, {Kind: "anonymous", Target: "/scratch"}}}
	body, _ := json.Marshal(spec)
	w := request("POST", workload.APIPrefix+"/workloads", string(body), token, 201)
	var created workload.Item
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != "running" || created.Spec.Env != nil || strings.Contains(w.Body.String(), "test-only-env-value") {
		t.Fatal("workload did not launch or exposed environment values")
	}
	var persisted string
	if err := db.QueryRow(ctx, `SELECT spec::text FROM hub_children WHERE id=$1`, created.ID).Scan(&persisted); err != nil || strings.Contains(persisted, "test-only-env-value") {
		t.Fatal("environment content entered PostgreSQL")
	}
	fake.mu.Lock()
	secret := fake.stored[k.endpoint("secrets", childResource(created.ID))[len(k.base):]]
	value := secret["data"].(object)["PASSWORD"].(string)
	fake.mu.Unlock()
	if value != base64.StdEncoding.EncodeToString([]byte("test-only-env-value")) {
		t.Fatal("environment values did not reach Kubernetes Secret")
	}
	request("GET", workload.APIPrefix+"/workloads/database", "", otherToken, 404)
	request("DELETE", workload.APIPrefix+"/volumes/data", "", token, 409)
	request("POST", workload.APIPrefix+"/volumes", `{"name":"too-large","size":"2Gi"}`, token, 409)
	request("DELETE", "/api/workspaces/"+parent, "", a.token, 409)
	request("POST", control(parent, "undeploy"), "", a.token, 409)
	request("PATCH", control(parent, "owner"), `{"owner":"`+other+`"}`, a.token, 409)
	request("POST", workload.APIPrefix+"/workloads", string(body), token, 409)
	exec := request("POST", workload.APIPrefix+"/workloads/database/exec", `{"command":["sh","-c","echo 'a b'; exit 7"]}`, token, 200)
	var result workload.ExecResult
	_ = json.Unmarshal(exec.Body.Bytes(), &result)
	if result.ExitCode != 7 || result.Stdout != "output\n" || result.Stderr != "warning\n" {
		t.Fatalf("incorrect exec result: %+v", result)
	}
	fake.mu.Lock()
	args := append([]string{}, fake.execArgs...)
	fake.mu.Unlock()
	if !reflect.DeepEqual(args, []string{"sh", "-c", "echo 'a b'; exit 7"}) {
		t.Fatal("exec command was changed or implicitly shell-quoted")
	}
	request("GET", workload.APIPrefix+"/workloads/database/logs", "", token, 200)
	// Exercise the actual typed client against the implemented TLS API.
	hub := httptest.NewTLSServer(http.HandlerFunc(a.api))
	defer hub.Close()
	client, err := hubclient.New(hub.URL, token, hub.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	var items []workload.Item
	if err := client.Do(ctx, "GET", "/workloads", nil, &items); err != nil || len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("typed client cannot use backend: %v, %+v", err, items)
	}
	second := workload.Spec{Name: "second", Image: "alpine:3.22", CPU: "500m", Memory: "512Mi"}
	// Two concurrent requests compete for the last resource reservation. Only
	// one can succeed, even though they use different names.
	type outcome struct {
		name string
		code int
	}
	outcomes := make(chan outcome, 2)
	var concurrent sync.WaitGroup
	for _, name := range []string{"second", "third"} {
		concurrent.Add(1)
		go func(name string) {
			defer concurrent.Done()
			s := second
			s.Name = name
			data, _ := json.Marshal(s)
			r := httptest.NewRequest("POST", workload.APIPrefix+"/workloads", strings.NewReader(string(data)))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			a.api(w, r)
			outcomes <- outcome{name, w.Code}
		}(name)
	}
	concurrent.Wait()
	close(outcomes)
	winner := ""
	for result := range outcomes {
		switch result.code {
		case 201:
			if winner != "" {
				t.Fatal("concurrent requests exceeded the resource budget")
			}
			winner = result.name
		case 409:
		default:
			t.Fatalf("unexpected concurrent result %d", result.code)
		}
	}
	if winner == "" {
		t.Fatal("neither request obtained the available reservation")
	}
	request("PUT", control(parent, "workload-access"), strings.Replace(budget, `"cpu":"1"`, `"cpu":"500m"`, 1), a.token, 409)
	second.Name = "overflow"
	body, _ = json.Marshal(second)
	request("POST", workload.APIPrefix+"/workloads", string(body), token, 409)
	fake.mu.Lock()
	fake.delayPods = true
	fake.mu.Unlock()
	request("DELETE", workload.APIPrefix+"/workloads/database", "", token, 502)
	request("POST", workload.APIPrefix+"/workloads", string(body), token, 409)
	fake.mu.Lock()
	fake.delayPods = false
	fake.mu.Unlock()
	request("DELETE", workload.APIPrefix+"/workloads/database", "", token, 204)
	var volumes int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM hub_child_volumes WHERE parent=$1`, parent).Scan(&volumes); err != nil || volumes != 1 {
		t.Fatal("anonymous volume was not removed, or named volume was removed")
	}
	request("DELETE", workload.APIPrefix+"/workloads/"+winner, "", token, 204)
	// A provisioned volume must not be silently recreated after external loss.
	v, err := a.loadChildVolume(ctx, parent, "data")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	pvcKey := strings.TrimPrefix(k.endpoint("persistentvolumeclaims", childResource(v.ID)), k.base)
	delete(fake.stored, pvcKey)
	fake.mu.Unlock()
	if err := a.syncChildVolume(ctx, v); err == nil {
		t.Fatal("recreated a missing previously provisioned volume")
	}
	fake.mu.Lock()
	if fake.stored[pvcKey] != nil {
		t.Fatal("data-loss guard failed")
	}
	fake.mu.Unlock()
	request("DELETE", workload.APIPrefix+"/volumes/data", "", token, 204)
	// A missing previously launched Pod is marked lost, never rerun.
	second.Name, second.AutoRemove = "once", false
	body, _ = json.Marshal(second)
	request("POST", workload.APIPrefix+"/workloads", string(body), token, 201)
	once, err := a.loadChild(ctx, parent, "once")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	podKey := strings.TrimPrefix(k.endpoint("pods", childResource(once.ID)), k.base)
	delete(fake.stored, podKey)
	fake.mu.Unlock()
	if err := a.reconcileChildren(ctx); err != nil {
		t.Fatal(err)
	}
	once, _ = a.loadChild(ctx, parent, "once")
	if once.Status != "lost" {
		t.Fatalf("missing executed Pod was not marked lost: %s", once.Status)
	}
	if _, err := db.Exec(ctx, `UPDATE hub_children SET expires_at=now()-interval '1 second' WHERE id=$1`, once.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileChildren(ctx); err != nil {
		t.Fatal(err)
	}
	request("GET", workload.APIPrefix+"/workloads/once", "", token, 404)
	// Automatic cleanup observes completion, then removes anonymous storage
	// independently of any CLI process remaining connected.
	second.Name, second.AutoRemove = "auto", true
	second.Mounts = []workload.Mount{{Kind: "anonymous", Target: "/scratch"}}
	body, _ = json.Marshal(second)
	request("POST", workload.APIPrefix+"/workloads", string(body), token, 201)
	auto, _ := a.loadChild(ctx, parent, "auto")
	fake.mu.Lock()
	fake.stored[strings.TrimPrefix(k.endpoint("pods", childResource(auto.ID)), k.base)]["status"] = object{
		"phase": "Succeeded", "containerStatuses": []object{{"name": "workload", "state": object{"terminated": object{"exitCode": 0}}}},
	}
	fake.mu.Unlock()
	for range 2 {
		if err := a.reconcileChildren(ctx); err != nil {
			t.Fatal(err)
		}
	}
	request("GET", workload.APIPrefix+"/workloads/auto", "", token, 404)
	if err := db.QueryRow(ctx, `SELECT count(*) FROM hub_child_volumes WHERE parent=$1`, parent).Scan(&volumes); err != nil || volumes != 0 {
		t.Fatal("auto-remove retained anonymous storage")
	}
	newToken := mint(parent)
	request("GET", workload.APIPrefix+"/workloads", "", token, 401)
	request("GET", workload.APIPrefix+"/workloads", "", newToken, 200)
	request("DELETE", control(parent, "workload-token"), "", a.token, 204)
	request("GET", workload.APIPrefix+"/workloads", "", newToken, 401)
	expiring := mint(parent)
	if _, err := db.Exec(ctx, `UPDATE hub_child_tokens SET expires_at=now()-interval '1 second' WHERE parent=$1`, parent); err != nil {
		t.Fatal(err)
	}
	request("GET", workload.APIPrefix+"/workloads", "", expiring, 401)
	disabled := mint(parent)
	request("PUT", control(parent, "workload-access"), strings.Replace(budget, `"enabled":true`, `"enabled":false`, 1), a.token, 200)
	request("GET", workload.APIPrefix+"/workloads", "", disabled, 401)
}

func TestChildResourceOwnership(t *testing.T) {
	ctx := context.Background()
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			desired := childObject("Secret", "mine", "parent", nil)
			delete(desired, "spec")
			existing := childObject("Secret", "mine", "parent", nil)
			if foreign {
				existing["metadata"].(object)["labels"].(object)["herdr-hub/child-id"] = "someone-else"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("create-only ownership check overwrote or deleted a resource")
				}
				writeJSON(w, 200, existing)
			}))
			defer server.Close()
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("test-only"), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kubernetes{base: server.URL, namespace: "test", tokenFile: tokenFile, client: server.Client()}
			err := k.ensureChild(ctx, desired)
			if foreign != (err != nil) {
				t.Fatalf("wrong ownership result: %v", err)
			}
			if foreign && k.deleteChildObject(ctx, "secrets", childResource("mine"), "mine", "parent") == nil {
				t.Fatal("foreign managed resource could be removed")
			}
		})
	}
}

func TestExecRejectsInvalidTerminalStatus(t *testing.T) {
	for _, frame := range [][]byte{
		{9}, {3, '{'}, append([]byte{3}, []byte(`{"status":"Failure"}`)...),
		append([]byte{3}, []byte(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"999"}]}}`)...),
		append([]byte{1}, make([]byte, (1<<20)+1)...),
	} {
		t.Run(fmt.Sprint(len(frame), "-", frame[0]), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v4.channel.k8s.io"}})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_ = conn.Write(r.Context(), websocket.MessageBinary, frame)
			}))
			defer server.Close()
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("test-only"), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kubernetes{base: server.URL, namespace: "test", tokenFile: tokenFile, client: server.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := k.execChild(ctx, childRecord{Item: workload.Item{ID: "test"}}, workload.ExecRequest{Command: []string{"true"}}); err == nil {
				t.Fatal("accepted invalid or oversized exec output")
			}
		})
	}
}

func TestChildOpenAPIContract(t *testing.T) {
	var spec object
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	paths := spec["paths"].(object)
	for path, methods := range map[string][]string{
		"/api/workspaces/{name}/workload-access":      {"get", "put"},
		"/api/workspaces/{name}/workload-token":       {"post", "delete"},
		workload.APIPrefix + "/workloads":             {"get", "post"},
		workload.APIPrefix + "/workloads/{name}":      {"get", "delete"},
		workload.APIPrefix + "/workloads/{name}/logs": {"get"},
		workload.APIPrefix + "/workloads/{name}/exec": {"post"},
		workload.APIPrefix + "/volumes":               {"get", "post"},
		workload.APIPrefix + "/volumes/{name}":        {"get", "delete"},
	} {
		p, ok := paths[path].(object)
		if !ok {
			t.Fatalf("missing OpenAPI path %s", path)
		}
		for _, method := range methods {
			op, ok := p[method].(object)
			if !ok {
				t.Fatalf("missing OpenAPI operation %s %s", method, path)
			}
			if strings.HasPrefix(path, workload.APIPrefix) {
				security, _ := op["security"].([]any)
				if len(security) != 1 || security[0].(object)["workloadAuth"] == nil {
					t.Fatalf("workload operation advertises admin authentication: %s %s", method, path)
				}
			}
		}
	}
	schemas := spec["components"].(object)["schemas"].(object)
	for _, test := range []struct {
		schema string
		value  any
	}{
		{"ChildWorkloadSpec", workload.Spec{}}, {"ChildMount", workload.Mount{}}, {"ChildWorkload", workload.Item{}},
		{"ChildVolume", workload.Volume{}}, {"ChildExecRequest", workload.ExecRequest{}}, {"ChildExecResult", workload.ExecResult{}},
		{"ChildBudget", childBudget{}},
	} {
		properties := schemas[test.schema].(object)["properties"].(object)
		typ := reflect.TypeOf(test.value)
		if len(properties) != typ.NumField() {
			t.Fatalf("%s property count differs from Go wire type", test.schema)
		}
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if properties[name] == nil {
				t.Fatalf("%s missing Go wire property %s", test.schema, name)
			}
		}
	}
}
