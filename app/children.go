package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/Werner2005/herdr-hub/internal/workload"
	"github.com/jackc/pgx/v5"
)

type childMount struct {
	VolumeID string `json:"volume_id,omitempty"`
	SubPath  string `json:"sub_path,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	Home     bool   `json:"home,omitempty"`
}

type childRecord struct {
	workload.Item
	Parent    string
	Mounts    []childMount
	ExpiresAt time.Time
}

func (a *application) loadChild(ctx context.Context, parent, name string) (childRecord, error) {
	var c childRecord
	err := a.db.QueryRow(ctx, `SELECT id,parent,name,spec,mounts,status,error,exit_code,expires_at FROM hub_children WHERE parent=$1 AND name=$2`, parent, name).
		Scan(&c.ID, &c.Parent, &c.Name, &c.Spec, &c.Mounts, &c.Status, &c.Error, &c.ExitCode, &c.ExpiresAt)
	c.Hostname = childResource(c.ID) + "." + a.kube.namespace + ".svc"
	return c, err
}

func childMemory(value string) (*big.Rat, error) {
	q, err := usageQuantity(value)
	if err == nil && q.Sign() <= 0 {
		err = fmt.Errorf("memory must be positive")
	}
	return q, err
}

// Called only under the single-replica mutation lock; includes all pending,
// completed, failed and deleting reservations until cleanup is confirmed.
func (a *application) checkChildBudget(ctx context.Context, parent string, b childBudget, extra *workload.Spec, volumes, storage int) error {
	cpu, memory := new(big.Rat), new(big.Rat)
	count := 0
	add := func(s workload.Spec) error {
		c, err := resourceQuantity(s.CPU, true)
		if err != nil {
			return err
		}
		m, err := childMemory(s.Memory)
		if err != nil {
			return err
		}
		cpu.Add(cpu, c)
		memory.Add(memory, m)
		count++
		return nil
	}
	if extra != nil {
		if err := add(*extra); err != nil {
			return err
		}
	}
	rows, err := a.db.Query(ctx, `SELECT spec FROM hub_children WHERE parent=$1`, parent)
	if err != nil {
		return err
	}
	for rows.Next() {
		var spec workload.Spec
		if err = rows.Scan(&spec); err != nil {
			break
		}
		if err = add(spec); err != nil {
			break
		}
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return fmt.Errorf("could not read workload reservations")
	}
	maxCPU, err := resourceQuantity(b.CPU, true)
	if err != nil {
		return err
	}
	maxMemory, err := resourceQuantity(b.Memory, false)
	if err != nil {
		return err
	}
	if count > b.MaxWorkloads || cpu.Cmp(maxCPU) > 0 || memory.Cmp(maxMemory) > 0 {
		return fmt.Errorf("combined workload count, CPU or memory budget exceeded")
	}
	rows, err = a.db.Query(ctx, `SELECT size FROM hub_child_volumes WHERE parent=$1`, parent)
	if err != nil {
		return err
	}
	for rows.Next() {
		var size string
		if err = rows.Scan(&size); err != nil {
			break
		}
		n, e := storageGi(size)
		if e != nil {
			err = e
			break
		}
		volumes++
		storage += n
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return fmt.Errorf("could not read storage reservations")
	}
	if volumes > b.MaxVolumes || storage > b.StorageGi {
		return fmt.Errorf("combined volume count or storage budget exceeded")
	}
	return nil
}

func (a *application) childrenAPI(w http.ResponseWriter, r *http.Request, u childIdentity, parts []string) {
	if len(parts) == 1 {
		switch r.Method {
		case "GET":
			rows, err := a.db.Query(r.Context(), `SELECT name FROM hub_children WHERE parent=$1 ORDER BY name`, u.Parent)
			if err != nil {
				writeError(w, 500, "could not list workloads")
				return
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
			if err != nil || rows.Err() != nil {
				writeError(w, 500, "could not read workloads")
				return
			}
			result := []workload.Item{}
			for _, name := range names {
				item, e := a.loadChild(r.Context(), u.Parent, name)
				if e != nil {
					writeError(w, 500, "could not read workload")
					return
				}
				result = append(result, item.Item)
			}
			writeJSON(w, 200, result)
		case "POST":
			a.createChild(w, r, u)
		default:
			writeError(w, 405, "method not allowed")
		}
		return
	}
	item, err := a.loadChild(r.Context(), u.Parent, parts[1])
	if err != nil {
		writeError(w, 404, "workload not found")
		return
	}
	switch r.Method {
	case "GET":
		writeJSON(w, 200, item.Item)
	case "DELETE":
		if _, err = a.db.Exec(r.Context(), `UPDATE hub_children SET status='deleting' WHERE id=$1`, item.ID); err == nil {
			item.Status = "deleting"
			err = a.syncChild(r.Context(), item)
		}
		if err != nil {
			writeError(w, 502, "cleanup pending; reservations retained and reconciliation will retry")
			return
		}
		w.WriteHeader(204)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *application) createChild(w http.ResponseWriter, r *http.Request, u childIdentity) {
	var spec workload.Spec
	if !decodeChild(w, r, &spec) {
		return
	}
	if err := spec.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if spec.Stdin || spec.TTY {
		writeError(w, 400, "stdin/TTY sessions are not supported yet")
		return
	}
	if _, err := a.loadChild(r.Context(), u.Parent, spec.Name); err == nil {
		writeError(w, 409, "workload name already exists; inspect before retrying")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 500, "could not check workload name")
		return
	}
	id, err := childID()
	if err != nil {
		writeError(w, 500, "could not allocate workload ID")
		return
	}
	item := childRecord{Item: workload.Item{ID: id, Name: spec.Name, Spec: spec, Status: "preparing"}, Parent: u.Parent, Mounts: []childMount{}, ExpiresAt: time.Now().UTC().Add(8 * time.Hour)}
	// Values never enter PostgreSQL. A dedicated immutable Secret provides env
	// through envFrom and is retained for recovery until workload cleanup.
	envValues := spec.Env
	item.Spec.Env = nil
	newVolumes := []childVolume{}
	resolved := map[string]childVolume{}
	for _, mount := range spec.Mounts {
		cm := childMount{Target: mount.Target, ReadOnly: mount.ReadOnly}
		if mount.Kind == "workspace" {
			cm.Home, cm.SubPath = true, mount.Source
			// The kubelet's secure subPath handling rejects symlinks escaping
			// the PVC. The API never turns this into a node filesystem path.
			home, _, e := a.kube.call(r.Context(), "GET", "persistentvolumeclaims", resourceName(u.Parent), nil)
			if e != nil || !ownedParentPVC(home, u.Parent) {
				writeError(w, 409, "authorized parent home PVC is unavailable")
				return
			}
		} else {
			var v childVolume
			if mount.Kind == "named" {
				var ok bool
				v, ok = resolved[mount.Source]
				if !ok {
					v, err = a.loadChildVolume(r.Context(), u.Parent, mount.Source)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						writeError(w, 500, "could not resolve volume")
						return
					}
				}
				if v.Deleting || v.AnonymousFor != "" {
					writeError(w, 409, "named volume is unavailable")
					return
				}
			}
			if v.ID == "" {
				vid, e := childID()
				if e != nil {
					writeError(w, 500, "could not allocate volume ID")
					return
				}
				v = childVolume{Volume: workload.Volume{Name: mount.Source, Size: "1Gi"}, ID: vid, Parent: u.Parent}
				if mount.Kind == "anonymous" {
					v.Name, v.AnonymousFor = "anon-"+vid, id
				}
				newVolumes = append(newVolumes, v)
			}
			if mount.Kind == "named" {
				resolved[mount.Source] = v
			}
			cm.VolumeID = v.ID
		}
		item.Mounts = append(item.Mounts, cm)
	}
	if err := a.checkChildBudget(r.Context(), u.Parent, u.Budget, &spec, len(newVolumes), len(newVolumes)); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	for _, v := range newVolumes {
		if _, err = tx.Exec(r.Context(), `INSERT INTO hub_child_volumes(id,parent,name,size,anonymous_for) VALUES($1,$2,$3,$4,$5)`, v.ID, v.Parent, v.Name, v.Size, v.AnonymousFor); err != nil {
			break
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO hub_children(id,parent,name,spec,mounts,status,expires_at) VALUES($1,$2,$3,$4,$5,'preparing',$6)`, id, u.Parent, spec.Name, item.Spec, item.Mounts, item.ExpiresAt)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "could not reserve workload resources")
		return
	}
	// Missing env cannot be reconstructed from SQL. Preparing records without
	// a Secret remain errors rather than silently starting with missing values.
	if err = a.kube.ensureChild(r.Context(), childEnvSecret(item, envValues)); err == nil {
		_, err = a.db.Exec(r.Context(), `UPDATE hub_children SET status='pending' WHERE id=$1`, id)
	}
	if err == nil {
		item.Status = "pending"
		err = a.syncChild(r.Context(), item)
	}
	if err != nil {
		writeError(w, 502, "workload reserved; inspect before retrying, or remove it to release resources")
		return
	}
	result, err := a.loadChild(r.Context(), u.Parent, spec.Name)
	if err != nil {
		writeError(w, 500, "workload created but could not reload status")
		return
	}
	writeJSON(w, 201, result.Item)
}

type childVolume struct {
	workload.Volume
	ID, Parent, AnonymousFor string
	Deleting, Provisioned    bool
}

func (a *application) loadChildVolume(ctx context.Context, parent, name string) (childVolume, error) {
	var v childVolume
	err := a.db.QueryRow(ctx, `SELECT id,parent,name,size,anonymous_for,deleting,provisioned FROM hub_child_volumes WHERE parent=$1 AND name=$2`, parent, name).
		Scan(&v.ID, &v.Parent, &v.Name, &v.Size, &v.AnonymousFor, &v.Deleting, &v.Provisioned)
	return v, err
}

func (a *application) childVolumesAPI(w http.ResponseWriter, r *http.Request, u childIdentity, parts []string) {
	if len(parts) == 1 && r.Method == "GET" {
		rows, err := a.db.Query(r.Context(), `SELECT name,size FROM hub_child_volumes WHERE parent=$1 AND anonymous_for='' ORDER BY name`, u.Parent)
		if err != nil {
			writeError(w, 500, "could not list volumes")
			return
		}
		defer rows.Close()
		result := []workload.Volume{}
		for rows.Next() {
			var v workload.Volume
			if err = rows.Scan(&v.Name, &v.Size); err != nil {
				break
			}
			result = append(result, v)
		}
		if err != nil || rows.Err() != nil {
			writeError(w, 500, "could not read volumes")
			return
		}
		writeJSON(w, 200, result)
		return
	}
	if len(parts) == 1 && r.Method == "POST" {
		var input workload.Volume
		if !decodeChild(w, r, &input) {
			return
		}
		n, err := storageGi(input.Size)
		if !validName(input.Name) || err != nil {
			writeError(w, 400, "provide a valid volume name and positive whole Gi size")
			return
		}
		if _, err = a.loadChildVolume(r.Context(), u.Parent, input.Name); err == nil {
			writeError(w, 409, "volume already exists")
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 500, "could not check volume")
			return
		}
		if err = a.checkChildBudget(r.Context(), u.Parent, u.Budget, nil, 1, n); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		id, err := childID()
		if err == nil {
			_, err = a.db.Exec(r.Context(), `INSERT INTO hub_child_volumes(id,parent,name,size) VALUES($1,$2,$3,$4)`, id, u.Parent, input.Name, input.Size)
		}
		if err != nil {
			writeError(w, 500, "could not reserve storage")
			return
		}
		v := childVolume{Volume: input, ID: id, Parent: u.Parent}
		if err = a.syncChildVolume(r.Context(), v); err != nil {
			writeError(w, 502, "storage reserved; reconciliation will retry provisioning")
			return
		}
		writeJSON(w, 201, input)
		return
	}
	if len(parts) != 2 {
		writeError(w, 405, "method not allowed")
		return
	}
	v, err := a.loadChildVolume(r.Context(), u.Parent, parts[1])
	if err != nil || v.AnonymousFor != "" {
		writeError(w, 404, "volume not found")
		return
	}
	switch r.Method {
	case "GET":
		writeJSON(w, 200, v.Volume)
	case "DELETE":
		var used bool
		if err = a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM hub_children WHERE parent=$1 AND mounts @> $2::jsonb)`, u.Parent, []object{{"volume_id": v.ID}}).Scan(&used); err != nil {
			writeError(w, 500, "could not check volume references")
			return
		}
		if used {
			writeError(w, 409, "volume is still referenced by a workload")
			return
		}
		if _, err = a.db.Exec(r.Context(), `UPDATE hub_child_volumes SET deleting=true WHERE id=$1`, v.ID); err == nil {
			v.Deleting = true
			err = a.syncChildVolume(r.Context(), v)
		}
		if err != nil {
			writeError(w, 502, "storage cleanup pending; reservation retained")
			return
		}
		w.WriteHeader(204)
	default:
		writeError(w, 405, "method not allowed")
	}
}
