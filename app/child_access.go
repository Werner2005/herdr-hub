package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Werner2005/herdr-hub/internal/workload"
	"github.com/jackc/pgx/v5"
)

type childBudget struct {
	Enabled      bool   `json:"enabled"`
	MaxWorkloads int    `json:"max_workloads"`
	CPU          string `json:"cpu"`
	Memory       string `json:"memory"`
	StorageGi    int    `json:"storage_gi"`
	MaxVolumes   int    `json:"max_volumes"`
}

func workloadsEnabled() bool {
	return os.Getenv("WORKLOADS_ENABLED") == "true" && env("WORKSPACE_NETWORK_POLICY_ENABLED", "true") == "true"
}

func (b childBudget) validate() error {
	if b.MaxWorkloads < 1 || b.MaxWorkloads > 100 || b.MaxVolumes < 1 || b.MaxVolumes > 100 || b.StorageGi < 1 || b.StorageGi > 1048576 {
		return fmt.Errorf("use 1–100 workloads/volumes and 1–1048576 Gi storage")
	}
	for _, q := range []struct {
		value string
		cpu   bool
	}{{b.CPU, true}, {b.Memory, false}} {
		if _, err := resourceQuantity(q.value, q.cpu); err != nil {
			return err
		}
	}
	return nil
}

// Strict, bounded decoding rejects unknown fields AND multiple JSON documents.
func decodeChild(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid workload JSON")
		return false
	}
	return true
}

func childID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validChildToken(token string) bool {
	if len(token) != 68 || !strings.HasPrefix(token, "hhw_") {
		return false
	}
	_, err := hex.DecodeString(token[4:])
	return err == nil
}

type childIdentity struct {
	Parent, Owner string
	Budget        childBudget
}

func (a *application) authenticateChild(r *http.Request) (childIdentity, error) {
	var result childIdentity
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !validChildToken(token) {
		return result, pgx.ErrNoRows
	}
	err := a.db.QueryRow(r.Context(), `SELECT w.name,w.owner,b.budget FROM hub_child_tokens t
 JOIN workspaces w ON w.name=t.parent JOIN hub_users u ON u.username=w.owner
 JOIN hub_child_access b ON b.parent=w.name
 WHERE t.hash=$1 AND t.owner=w.owner AND t.expires_at>now()
 AND w.status NOT IN ('deleting','undeploying','undeployed')`, sessionHash(token)).Scan(&result.Parent, &result.Owner, &result.Budget)
	if err == nil && !result.Budget.Enabled {
		err = pgx.ErrNoRows
	}
	return result, err
}

// Called through the normal API's mutation lock and identity authentication.
// Only admins configure budgets. Owners/admins may issue or revoke one token.
func (a *application) childAccessAPI(w http.ResponseWriter, r *http.Request, parent, action string) {
	u := currentUser(r)
	var owner, state string
	if err := a.db.QueryRow(r.Context(), `SELECT owner,status FROM workspaces WHERE name=$1`, parent).Scan(&owner, &state); err != nil || (u.Role != "admin" && owner != u.Username) {
		writeError(w, 404, "workspace not found")
		return
	}
	if action == "workload-access" {
		switch r.Method {
		case "GET":
			budget := childBudget{MaxWorkloads: 5, CPU: "2", Memory: "4Gi", StorageGi: 20, MaxVolumes: 10}
			err := a.db.QueryRow(r.Context(), `SELECT budget FROM hub_child_access WHERE parent=$1`, parent).Scan(&budget)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				writeError(w, 500, "could not load workload access")
				return
			}
			writeJSON(w, 200, budget)
		case "PUT":
			if u.Role != "admin" {
				writeError(w, 403, "admin required to configure workload budgets")
				return
			}
			var budget childBudget
			if !decodeChild(w, r, &budget) {
				return
			}
			if err := budget.validate(); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			if budget.Enabled && (!workloadsEnabled() || state == "deleting" || state == "undeploying" || state == "undeployed" || owner == "") {
				writeError(w, 409, "workloads require an owned active workspace and enabled network policies/feature")
				return
			}
			// Do not shrink reservations out from under existing workloads.
			if err := a.checkChildBudget(r.Context(), parent, budget, nil, 0, 0); err != nil {
				writeError(w, 409, err.Error())
				return
			}
			_, err := a.db.Exec(r.Context(), `INSERT INTO hub_child_access(parent,budget) VALUES($1,$2)
 ON CONFLICT(parent) DO UPDATE SET budget=$2`, parent, budget)
			if err == nil && !budget.Enabled {
				_, err = a.db.Exec(r.Context(), `DELETE FROM hub_child_tokens WHERE parent=$1`, parent)
			}
			if err != nil {
				writeError(w, 500, "could not save workload access")
				return
			}
			writeJSON(w, 200, budget)
		default:
			writeError(w, 405, "method not allowed")
		}
		return
	}
	if r.Method == "DELETE" {
		if _, err := a.db.Exec(r.Context(), `DELETE FROM hub_child_tokens WHERE parent=$1`, parent); err != nil {
			writeError(w, 500, "could not revoke workload token")
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method != "POST" {
		writeError(w, 405, "method not allowed")
		return
	}
	var budget childBudget
	err := a.db.QueryRow(r.Context(), `SELECT budget FROM hub_child_access WHERE parent=$1`, parent).Scan(&budget)
	if err != nil || !budget.Enabled || !workloadsEnabled() || owner == "" || state == "deleting" || state == "undeploying" || state == "undeployed" {
		writeError(w, 409, "workload access is not enabled for this active workspace")
		return
	}
	first, err := childID()
	if err != nil {
		writeError(w, 500, "could not generate credential")
		return
	}
	second, err := childID()
	if err != nil {
		writeError(w, 500, "could not generate credential")
		return
	}
	token := "hhw_" + first + second
	expires := time.Now().UTC().Add(24 * time.Hour)
	_, err = a.db.Exec(r.Context(), `INSERT INTO hub_child_tokens(parent,owner,hash,expires_at) VALUES($1,$2,$3,$4)
 ON CONFLICT(parent) DO UPDATE SET owner=$2,hash=$3,expires_at=$4`, parent, owner, sessionHash(token), expires)
	if err != nil {
		writeError(w, 500, "could not save workload credential")
		return
	}
	writeJSON(w, 201, object{"token": token, "expires_at": expires})
}

// Parent lifecycle changes are refused while children or retained child volumes
// exist. This prevents handing another owner live sessions or private data.
func (a *application) childReferences(ctx context.Context, parent string) error {
	var exists bool
	err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hub_children WHERE parent=$1)
 OR EXISTS(SELECT 1 FROM hub_child_volumes WHERE parent=$1)`, parent).Scan(&exists)
	if err != nil {
		return fmt.Errorf("could not check child workload reservations")
	}
	if exists {
		return fmt.Errorf("remove child workloads and child volumes before changing the parent lifecycle or ownership")
	}
	return nil
}

// Scoped credentials are never passed through normal admin authentication.
// All operations recheck identity under the same lock as ownership changes.
func (a *application) childAPI(w http.ResponseWriter, r *http.Request) identity {
	if !workloadsEnabled() {
		writeError(w, 409, "child workloads are disabled or network policies are disabled")
		return identity{}
	}
	if !bufferRequestBody(w, r, 65536) {
		return identity{}
	}
	if err := a.mu.lock(r.Context()); err != nil {
		writeError(w, 503, "request expired")
		return identity{}
	}
	locked := true
	defer func() {
		if locked {
			a.mu.unlock()
		}
	}()
	u, err := a.authenticateChild(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 401, "scoped workload credential required")
		} else {
			writeError(w, 503, "workload authentication unavailable")
		}
		return identity{}
	}
	actor := identity{Username: u.Owner, Role: "workload"}
	p := strings.TrimPrefix(r.URL.Path, workload.APIPrefix+"/")
	parts := strings.Split(p, "/")
	if len(parts) < 1 || len(parts) > 3 || (len(parts) > 1 && !validName(parts[1])) {
		writeError(w, 404, "workload endpoint not found")
		return actor
	}
	if parts[0] == "volumes" && len(parts) <= 2 {
		a.childVolumesAPI(w, r, u, parts)
		return actor
	}
	if parts[0] != "workloads" {
		writeError(w, 404, "workload endpoint not found")
		return actor
	}
	if len(parts) == 3 {
		item, err := a.loadChild(r.Context(), u.Parent, parts[1])
		if err != nil {
			writeError(w, 404, "workload not found")
			return actor
		}
		if item.Status == "deleting" || item.Status == "preparing" || item.Status == "error" {
			writeError(w, 409, "workload is not available")
			return actor
		}
		if parts[2] != "logs" && parts[2] != "exec" {
			writeError(w, 404, "workload endpoint not found")
			return actor
		}
		var input workload.ExecRequest
		if parts[2] == "exec" {
			if r.Method != "POST" {
				writeError(w, 405, "method not allowed")
				return actor
			}
			if !decodeChild(w, r, &input) {
				return actor
			}
			s := workload.Spec{Name: "exec", Image: "exec", CPU: "1", Memory: "1Mi", Command: input.Command}
			if len(input.Command) == 0 || input.Command[0] == "" || s.Validate() != nil {
				writeError(w, 400, "provide a valid command argument array")
				return actor
			}
		} else if r.Method != "GET" {
			writeError(w, 405, "method not allowed")
			return actor
		}
		// Immutable resource IDs and lifecycle guards prevent targeting a new
		// owner's pod. Do not hold the mutation lock during bounded I/O.
		a.mu.unlock()
		locked = false
		a.childIO(w, r, item, parts[2], input)
		return actor
	}
	a.childrenAPI(w, r, u, parts)
	return actor
}
