package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

type sharedVolume struct {
	Name             string   `json:"name"`
	Owner            string   `json:"owner"`
	Size             string   `json:"storage_size"`
	Class            string   `json:"storage_class"`
	Users            []string `json:"users"`
	Groups           []string `json:"groups"`
	Status           string   `json:"status,omitempty"`
	AccessibleOwners []string `json:"accessible_owners,omitempty"`
}

var storageClassPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

type sharedGroup struct {
	Name    string   `json:"name"`
	Owner   string   `json:"owner"`
	Members []string `json:"members"`
}

func decodeShared(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		writeError(w, 400, "invalid JSON")
		return false
	}
	return true
}
func validNames(names []string, validate func(string) bool) bool {
	if len(names) > 100 {
		return false
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !validate(n) || seen[n] {
			return false
		}
		seen[n] = true
	}
	return true
}
func volumeClaim(name string) string { return "herdr-shared-" + name }

// Permission is evaluated for the workspace owner, even when an admin attaches.
const volumeAccessSQL = `SELECT EXISTS(SELECT 1 FROM shared_volumes v WHERE v.name=$1 AND (v.owner=$2 OR
 EXISTS(SELECT 1 FROM volume_users u WHERE u.volume=v.name AND u.username=$2) OR
 EXISTS(SELECT 1 FROM volume_groups g JOIN hub_group_members m ON m.group_name=g.group_name WHERE g.volume=v.name AND m.username=$2) OR
 EXISTS(SELECT 1 FROM volume_groups g JOIN hub_groups h ON h.name=g.group_name WHERE g.volume=v.name AND h.owner=$2)))`

func (a *application) volumeAccess(ctx context.Context, volume, user string) (bool, error) {
	var allowed bool
	err := a.db.QueryRow(ctx, volumeAccessSQL, volume, user).Scan(&allowed)
	return allowed, err
}
func (a *application) sharedAPI(w http.ResponseWriter, r *http.Request, parts []string) {
	validate := validName
	if parts[0] == "groups" {
		validate = validAccountName
	}
	if len(parts) > 3 || (len(parts) > 1 && !validate(parts[1])) {
		writeError(w, 404, "endpoint not found")
		return
	}
	if parts[0] == "groups" {
		a.groupsAPI(w, r, parts)
		return
	}
	if len(parts) == 1 && r.Method == "GET" {
		rows, err := a.db.Query(r.Context(), `SELECT name,owner,storage_size,storage_class,
   ARRAY(SELECT username FROM volume_users WHERE volume=v.name ORDER BY username),
   ARRAY(SELECT group_name FROM volume_groups WHERE volume=v.name ORDER BY group_name) FROM shared_volumes v ORDER BY name`)
		if err != nil {
			writeError(w, 500, "could not list volumes")
			return
		}
		volumes := []sharedVolume{}
		for rows.Next() {
			var v sharedVolume
			if err = rows.Scan(&v.Name, &v.Owner, &v.Size, &v.Class, &v.Users, &v.Groups); err != nil {
				break
			}
			volumes = append(volumes, v)
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			writeError(w, 500, "could not read volumes")
			return
		}
		result := []sharedVolume{}
		for _, v := range volumes {
			allowed, e := a.volumeAccess(r.Context(), v.Name, currentUser(r).Username)
			if e != nil {
				writeError(w, 500, "could not check volume access")
				return
			}
			if allowed || currentUser(r).Role == "admin" {
				if currentUser(r).Role == "admin" {
					accessRows, accessErr := a.db.Query(r.Context(), `SELECT owner FROM shared_volumes WHERE name=$1
 UNION SELECT username FROM volume_users WHERE volume=$1
 UNION SELECT m.username FROM volume_groups g JOIN hub_group_members m ON m.group_name=g.group_name WHERE g.volume=$1
 UNION SELECT h.owner FROM volume_groups g JOIN hub_groups h ON h.name=g.group_name WHERE g.volume=$1 ORDER BY 1`, v.Name)
					if accessErr != nil {
						writeError(w, 500, "could not list volume access")
						return
					}
					for accessRows.Next() {
						var username string
						if accessErr = accessRows.Scan(&username); accessErr != nil {
							break
						}
						v.AccessibleOwners = append(v.AccessibleOwners, username)
					}
					accessRows.Close()
					if accessErr != nil || accessRows.Err() != nil {
						writeError(w, 500, "could not read volume access")
						return
					}
				} else {
					v.AccessibleOwners = []string{currentUser(r).Username}
				}
				pvc, _, e := a.kube.call(r.Context(), "GET", "persistentvolumeclaims", volumeClaim(v.Name), nil)
				v.Status = "unavailable"
				if e == nil {
					status, _ := pvc["status"].(map[string]any)
					v.Status, _ = status["phase"].(string)
					if v.Status == "" {
						v.Status = "Pending"
					}
				}
				result = append(result, v)
			}
		}
		writeJSON(w, 200, result)
		return
	}
	if len(parts) == 1 && r.Method == "POST" {
		a.createVolume(w, r)
		return
	}
	if len(parts) < 2 {
		writeError(w, 404, "endpoint not found")
		return
	}
	var owner string
	err := a.db.QueryRow(r.Context(), `SELECT owner FROM shared_volumes WHERE name=$1`, parts[1]).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "volume not found")
		return
	}
	if err != nil {
		writeError(w, 500, "could not load volume")
		return
	}
	if owner != currentUser(r).Username && currentUser(r).Role != "admin" {
		writeError(w, 403, "volume owner or admin required")
		return
	}
	if len(parts) == 3 && parts[2] == "shares" && r.Method == "PUT" {
		a.shareVolume(w, r, parts[1])
		return
	}
	if len(parts) == 3 && parts[2] == "size" && r.Method == "PATCH" {
		a.resizeVolume(w, r, parts[1])
		return
	}
	if len(parts) == 3 && parts[2] == "owner" && r.Method == "PATCH" {
		a.changeVolumeOwner(w, r, parts[1], owner)
		return
	}
	if len(parts) == 2 && r.Method == "DELETE" {
		busy, err := a.volumeBusy(r.Context(), parts[1])
		if err != nil {
			writeError(w, 502, "could not inspect volume use")
			return
		}
		if busy {
			writeError(w, 409, "detach this volume and wait for old pods to terminate first")
			return
		}
		_, status, e := a.kube.call(r.Context(), "GET", "persistentvolumeclaims", volumeClaim(parts[1]), nil)
		if status != 404 {
			if e != nil {
				writeError(w, 502, "could not inspect shared PVC")
				return
			}
			if e = a.kube.checkSharedClaim(r.Context(), parts[1]); e != nil {
				writeError(w, 409, e.Error())
				return
			}
		}
		if err = a.kube.remove(r.Context(), "persistentvolumeclaims", volumeClaim(parts[1])); err != nil {
			writeError(w, 502, err.Error())
			return
		}
		if _, err = a.db.Exec(r.Context(), `DELETE FROM shared_volumes WHERE name=$1`, parts[1]); err != nil {
			writeError(w, 500, "PVC deletion requested; retry database cleanup")
			return
		}
		w.WriteHeader(204)
		return
	}
	writeError(w, 404, "endpoint not found")
}

func (a *application) createVolume(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Name  string `json:"name"`
		Owner string `json:"owner"`
		Size  string `json:"storage_size"`
		Class string `json:"storage_class"`
	}
	if !decodeShared(w, r, &v) {
		return
	}
	if !validName(v.Name) {
		writeError(w, 400, "use a lowercase DNS-label name, at most 40 characters")
		return
	}
	if _, err := storageGi(v.Size); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if v.Class != "" && (len(v.Class) > 253 || !storageClassPattern.MatchString(v.Class)) {
		writeError(w, 400, "invalid StorageClass name")
		return
	}
	if v.Owner == "" {
		v.Owner = currentUser(r).Username
	}
	if v.Owner == "" || (currentUser(r).Role != "admin" && v.Owner != currentUser(r).Username) {
		writeError(w, 403, "create a volume for your own account")
		return
	}
	if v.Class == "" {
		v.Class = a.kube.storageClass
	}
	if currentUser(r).Role != "admin" {
		if v.Class != a.kube.storageClass {
			writeError(w, 403, "use the configured workspace StorageClass")
			return
		}
		if err := a.checkStorageBudget(r.Context(), v.Owner, v.Name, v.Size); err != nil {
			writeError(w, 403, err.Error())
			return
		}
	}
	// Save before provisioning so failures are visible and the same POST can retry.
	_, err := a.db.Exec(r.Context(), `INSERT INTO shared_volumes(name,owner,storage_size,storage_class) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, v.Name, v.Owner, v.Size, v.Class)
	if err != nil {
		writeError(w, 400, "owner must be an existing user")
		return
	}
	var existing sharedVolume
	if err = a.db.QueryRow(r.Context(), `SELECT owner,storage_size,storage_class FROM shared_volumes WHERE name=$1`, v.Name).Scan(&existing.Owner, &existing.Size, &existing.Class); err != nil {
		writeError(w, 500, "could not load volume")
		return
	}
	if existing.Owner != v.Owner || existing.Size != v.Size || existing.Class != v.Class {
		writeError(w, 409, "volume name already in use with different settings")
		return
	}
	spec := object{"accessModes": []string{"ReadWriteMany"}, "resources": object{"requests": object{"storage": v.Size}}}
	if v.Class != "" {
		spec["storageClassName"] = v.Class
	}
	pvc := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": object{"name": volumeClaim(v.Name), "namespace": a.kube.namespace, "labels": object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/shared-volume": v.Name}}, "spec": spec}
	_, status, e := a.kube.call(r.Context(), "GET", "persistentvolumeclaims", volumeClaim(v.Name), nil)
	if status != 404 {
		if e != nil {
			writeError(w, 502, "could not inspect shared PVC; retry creation")
			return
		}
		if e = a.kube.checkSharedClaim(r.Context(), v.Name); e != nil {
			writeError(w, 409, e.Error())
			return
		}
	}
	if err = a.kube.ensure(r.Context(), pvc); err != nil {
		writeError(w, 502, "volume saved; retry creation with the same settings: "+err.Error())
		return
	}
	if err = a.kube.checkSharedClaim(r.Context(), v.Name); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	writeJSON(w, 201, sharedVolume{Name: v.Name, Owner: v.Owner, Size: v.Size, Class: v.Class, Users: []string{}, Groups: []string{}})
}

func (a *application) changeVolumeOwner(w http.ResponseWriter, r *http.Request, name, previousOwner string) {
	if currentUser(r).Role != "admin" {
		writeError(w, 403, "admin required")
		return
	}
	var input struct {
		Owner string `json:"owner"`
	}
	if !decodeShared(w, r, &input) {
		return
	}
	if !validAccountName(input.Owner) {
		writeError(w, 400, "provide a valid owner")
		return
	}
	var exists bool
	if err := a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM hub_users WHERE username=$1)`, input.Owner).Scan(&exists); err != nil {
		writeError(w, 500, "could not check owner")
		return
	}
	if !exists {
		writeError(w, 400, "owner must be an existing user")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	// Retain the previous owner's access so active mounts and assignments remain valid.
	if previousOwner != input.Owner {
		_, err = tx.Exec(r.Context(), `INSERT INTO volume_users(volume,username) VALUES($1,$2) ON CONFLICT DO NOTHING`, name, previousOwner)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE shared_volumes SET owner=$2 WHERE name=$1`, name, input.Owner)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "could not change volume owner")
		return
	}
	writeJSON(w, 200, input)
}

func (a *application) resizeVolume(w http.ResponseWriter, r *http.Request, name string) {
	var input struct {
		Size string `json:"storage_size"`
	}
	if !decodeShared(w, r, &input) {
		return
	}
	requested, err := storageGi(input.Size)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var savedSize string
	if err = a.db.QueryRow(r.Context(), `SELECT storage_size FROM shared_volumes WHERE name=$1`, name).Scan(&savedSize); err != nil {
		writeError(w, 500, "could not load volume size")
		return
	}
	saved, err := storageGi(savedSize)
	if err != nil {
		writeError(w, 409, "existing shared volume must use whole Gi sizes")
		return
	}
	pvc, err := a.kube.get(r.Context(), "persistentvolumeclaims", volumeClaim(name))
	if err != nil {
		writeError(w, 502, "shared PVC unavailable; retry creation before expanding")
		return
	}
	if err = validateSharedClaim(pvc, name); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	spec, _ := pvc["spec"].(map[string]any)
	resources, _ := spec["resources"].(map[string]any)
	requests, _ := resources["requests"].(map[string]any)
	size, _ := requests["storage"].(string)
	current, err := storageGi(size)
	if err != nil {
		writeError(w, 409, "existing shared PVC must use whole Gi sizes")
		return
	}
	// Check the PVC too: a previous expansion may have succeeded before the
	// database update failed. Retrying must never shrink the actual claim.
	if requested < saved || requested < current {
		writeError(w, 400, "shared volumes cannot shrink")
		return
	}
	if requested > current {
		if currentUser(r).Role != "admin" {
			if err = a.checkStorageBudget(r.Context(), currentUser(r).Username, name, input.Size); err != nil {
				writeError(w, 403, err.Error())
				return
			}
		}
		// Reserve capacity before changing Kubernetes. A failed database save
		// after a successful PVC expansion must not allow budget oversubscription.
		if _, err = a.db.Exec(r.Context(), `UPDATE shared_volumes SET storage_size=$2 WHERE name=$1`, name, input.Size); err != nil {
			writeError(w, 500, "could not reserve storage budget")
			return
		}
		requests["storage"] = input.Size
		delete(pvc, "status")
		if _, _, err = a.kube.call(r.Context(), "PUT", "persistentvolumeclaims", volumeClaim(name), pvc); err != nil {
			writeError(w, 502, "storage budget reserved; retry expansion (StorageClass must allow expansion): "+err.Error())
			return
		}
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE shared_volumes SET storage_size=$2 WHERE name=$1`, name, input.Size); err != nil {
		writeError(w, 500, "volume updated; retry to save database size")
		return
	}
	writeJSON(w, 200, input)
}

func (a *application) shareVolume(w http.ResponseWriter, r *http.Request, name string) {
	var input struct {
		Users  []string `json:"users"`
		Groups []string `json:"groups"`
	}
	if !decodeShared(w, r, &input) {
		return
	}
	if input.Users == nil || input.Groups == nil || !validNames(input.Users, validAccountName) || !validNames(input.Groups, validAccountName) {
		writeError(w, 400, "provide unique users and groups arrays")
		return
	}
	var removing bool
	if err := a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM volume_users WHERE volume=$1 AND NOT(username=ANY($2::text[]))) OR EXISTS(SELECT 1 FROM volume_groups WHERE volume=$1 AND NOT(group_name=ANY($3::text[])))`, name, input.Users, input.Groups).Scan(&removing); err != nil {
		writeError(w, 500, "could not check shares")
		return
	}
	if removing {
		busy, err := a.volumeBusy(r.Context(), name)
		if err != nil {
			writeError(w, 502, "could not inspect volume use")
			return
		}
		if busy {
			writeError(w, 409, "detach this volume from all workspaces and wait for old pods before removing shares")
			return
		}
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `DELETE FROM volume_users WHERE volume=$1;`, name)
	if err == nil {
		_, err = tx.Exec(r.Context(), `DELETE FROM volume_groups WHERE volume=$1`, name)
	}
	for _, u := range input.Users {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO volume_users VALUES($1,$2)`, name, u)
		}
	}
	for _, g := range input.Groups {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO volume_groups VALUES($1,$2)`, name, g)
		}
	}
	if err != nil {
		writeError(w, 400, "all users and groups must exist")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "could not save shares")
		return
	}
	writeJSON(w, 200, input)
}

func (a *application) groupsAPI(w http.ResponseWriter, r *http.Request, parts []string) {
	u := currentUser(r)
	if len(parts) == 1 && r.Method == "GET" {
		rows, err := a.db.Query(r.Context(), `SELECT name,owner,ARRAY(SELECT username FROM hub_group_members WHERE group_name=g.name ORDER BY username) FROM hub_groups g WHERE $1='admin' OR owner=$2 OR EXISTS(SELECT 1 FROM hub_group_members WHERE group_name=g.name AND username=$2) ORDER BY name`, u.Role, u.Username)
		if err != nil {
			writeError(w, 500, "could not list groups")
			return
		}
		defer rows.Close()
		result := []sharedGroup{}
		for rows.Next() {
			var g sharedGroup
			if rows.Scan(&g.Name, &g.Owner, &g.Members) != nil {
				writeError(w, 500, "could not read groups")
				return
			}
			result = append(result, g)
		}
		if rows.Err() != nil {
			writeError(w, 500, "could not read groups")
			return
		}
		writeJSON(w, 200, result)
		return
	}
	if len(parts) == 1 && r.Method == "POST" {
		var input struct {
			Name  string `json:"name"`
			Owner string `json:"owner"`
		}
		if !decodeShared(w, r, &input) {
			return
		}
		if !validAccountName(input.Name) {
			writeError(w, 400, "group name must start with a letter, contain letters, digits or internal hyphens, and be at most 40 characters")
			return
		}
		if input.Owner == "" {
			input.Owner = u.Username
		}
		if input.Owner == "" || (u.Role != "admin" && input.Owner != u.Username) {
			writeError(w, 403, "create a group for your own account")
			return
		}
		if _, err := a.db.Exec(r.Context(), `INSERT INTO hub_groups VALUES($1,$2)`, input.Name, input.Owner); err != nil {
			writeError(w, 409, "group exists or owner does not exist")
			return
		}
		writeJSON(w, 201, sharedGroup{Name: input.Name, Owner: input.Owner, Members: []string{}})
		return
	}
	if len(parts) < 2 {
		writeError(w, 404, "endpoint not found")
		return
	}
	var owner string
	err := a.db.QueryRow(r.Context(), `SELECT owner FROM hub_groups WHERE name=$1`, parts[1]).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "group not found")
		return
	}
	if err != nil {
		writeError(w, 500, "could not load group")
		return
	}
	if owner != u.Username && u.Role != "admin" {
		writeError(w, 403, "group owner or admin required")
		return
	}
	deleting := len(parts) == 2 && r.Method == "DELETE"
	if !deleting && !(len(parts) == 3 && parts[2] == "members" && r.Method == "PUT") {
		writeError(w, 404, "endpoint not found")
		return
	}
	input := struct {
		Members []string `json:"members"`
	}{Members: []string{}}
	if !deleting && !decodeShared(w, r, &input) {
		return
	}
	if input.Members == nil || !validNames(input.Members, validAccountName) {
		writeError(w, 400, "provide a unique members array")
		return
	}
	var removing bool
	if err = a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM hub_group_members WHERE group_name=$1 AND NOT(username=ANY($2::text[])))`, parts[1], input.Members).Scan(&removing); err != nil {
		writeError(w, 500, "could not check members")
		return
	}
	if deleting || removing {
		rows, e := a.db.Query(r.Context(), `SELECT volume FROM volume_groups WHERE group_name=$1`, parts[1])
		if e != nil {
			writeError(w, 500, "could not check group shares")
			return
		}
		names := []string{}
		for rows.Next() {
			var n string
			if e = rows.Scan(&n); e != nil {
				break
			}
			names = append(names, n)
		}
		rows.Close()
		if e != nil || rows.Err() != nil {
			writeError(w, 500, "could not read group shares")
			return
		}
		for _, n := range names {
			busy, e := a.volumeBusy(r.Context(), n)
			if e != nil {
				writeError(w, 502, "could not inspect group volume use")
				return
			}
			if busy {
				writeError(w, 409, "detach group-shared volumes and wait for old pods before removing members or deleting the group")
				return
			}
		}
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	if deleting {
		_, err = tx.Exec(r.Context(), `DELETE FROM hub_groups WHERE name=$1`, parts[1])
	} else {
		_, err = tx.Exec(r.Context(), `DELETE FROM hub_group_members WHERE group_name=$1`, parts[1])
		for _, m := range input.Members {
			if err == nil {
				_, err = tx.Exec(r.Context(), `INSERT INTO hub_group_members VALUES($1,$2)`, parts[1], m)
			}
		}
	}
	if err != nil {
		writeError(w, 400, "all group members must be existing users")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "could not save group")
		return
	}
	if deleting {
		w.WriteHeader(204)
		return
	}
	writeJSON(w, 200, input)
}

func (a *application) volumeBusy(ctx context.Context, name string) (bool, error) {
	var busy bool
	if err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE volumes ? $1)`, name).Scan(&busy); err != nil || busy {
		return busy, err
	}
	pods, err := a.kube.sharedPods(ctx)
	if err != nil {
		return false, err
	}
	for _, raw := range pods {
		pod, _ := raw.(map[string]any)
		if podUsesClaim(pod, volumeClaim(name)) {
			return true, nil
		}
	}
	return false, nil
}

func (a *application) mountVolumes(w http.ResponseWriter, r *http.Request, name string) {
	var input struct {
		Volumes []string `json:"volumes"`
	}
	if !decodeShared(w, r, &input) {
		return
	}
	if input.Volumes == nil || !validNames(input.Volumes, validName) || len(input.Volumes) > 16 {
		writeError(w, 400, "provide up to 16 unique volume names")
		return
	}
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Status == "deleting" || item.Status == "undeployed" || item.Status == "undeploying" {
		writeError(w, 409, "deploy the workspace before updating shared mounts")
		return
	}
	for _, v := range input.Volumes {
		allowed, e := a.volumeAccess(r.Context(), v, item.Owner)
		if e != nil {
			writeError(w, 500, "could not check volume access")
			return
		}
		if !allowed {
			writeError(w, 403, "workspace owner does not have access to volume "+v)
			return
		}
		if e = a.kube.checkSharedClaim(r.Context(), v); e != nil {
			writeError(w, 409, e.Error())
			return
		}
	}
	// Reserve both old and new mounts before Kubernetes changes. On failure these
	// conservative reservations prevent deletion/revocation; repeating PUT repairs.
	reserved := append([]string{}, item.Volumes...)
	for _, v := range input.Volumes {
		if !slices.Contains(reserved, v) {
			reserved = append(reserved, v)
		}
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET volumes=$2 WHERE name=$1`, name, reserved); err != nil {
		writeError(w, 500, "could not reserve volume mounts")
		return
	}
	if err = a.kube.updateSharedMounts(r.Context(), name, input.Volumes); err != nil {
		writeError(w, 502, "mount update incomplete; retry: "+err.Error())
		return
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET volumes=$2,status='provisioning',error='' WHERE name=$1`, name, input.Volumes); err != nil {
		writeError(w, 500, "mounts updated; retry to save state")
		return
	}
	writeJSON(w, 200, input)
}

func (k *kubernetes) checkSharedClaim(ctx context.Context, name string) error {
	pvc, err := k.get(ctx, "persistentvolumeclaims", volumeClaim(name))
	if err != nil {
		return fmt.Errorf("shared PVC %s unavailable; retry creation", name)
	}
	return validateSharedClaim(pvc, name)
}

func validateSharedClaim(pvc object, name string) error {
	meta, _ := pvc["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	if meta["deletionTimestamp"] != nil || labels["app.kubernetes.io/managed-by"] != "herdr-hub" || labels["herdr-hub/shared-volume"] != name {
		return fmt.Errorf("shared PVC %s is deleting or not owned by Herdr", name)
	}
	spec, _ := pvc["spec"].(map[string]any)
	modes, _ := spec["accessModes"].([]any)
	if !slices.Contains(modes, any("ReadWriteMany")) {
		return fmt.Errorf("shared PVC %s must support ReadWriteMany", name)
	}
	return nil
}

// Mount paths and volume names are fixed, so shared data cannot shadow home,
// credentials, or system files. This helper handles generated and decoded pods.
func addSharedMounts(pod object, names []string) {
	switch volumes := pod["volumes"].(type) {
	case []object:
		for _, n := range names {
			volumes = append(volumes, object{"name": "shared-" + n, "persistentVolumeClaim": object{"claimName": volumeClaim(n)}})
		}
		pod["volumes"] = volumes
	case []any:
		kept := []any{}
		for _, raw := range volumes {
			v, _ := raw.(map[string]any)
			s, _ := v["name"].(string)
			if !strings.HasPrefix(s, "shared-") {
				kept = append(kept, raw)
			}
		}
		for _, n := range names {
			kept = append(kept, object{"name": "shared-" + n, "persistentVolumeClaim": object{"claimName": volumeClaim(n)}})
		}
		pod["volumes"] = kept
	}
	mount := func(c object) {
		switch mounts := c["volumeMounts"].(type) {
		case []object:
			for _, n := range names {
				mounts = append(mounts, object{"name": "shared-" + n, "mountPath": "/mnt/shared/" + n})
			}
			c["volumeMounts"] = mounts
		case []any:
			kept := []any{}
			for _, raw := range mounts {
				m, _ := raw.(map[string]any)
				s, _ := m["name"].(string)
				if !strings.HasPrefix(s, "shared-") {
					kept = append(kept, raw)
				}
			}
			for _, n := range names {
				kept = append(kept, object{"name": "shared-" + n, "mountPath": "/mnt/shared/" + n})
			}
			c["volumeMounts"] = kept
		}
	}
	switch cs := pod["containers"].(type) {
	case []object:
		for _, c := range cs {
			if c["name"] == "herdr" {
				mount(c)
			}
		}
	case []any:
		for _, raw := range cs {
			c, _ := raw.(map[string]any)
			if c["name"] == "herdr" {
				mount(c)
			}
		}
	}
}
func (k *kubernetes) updateSharedMounts(ctx context.Context, name string, names []string) error {
	for attempt := 0; attempt < 5; attempt++ {
		d, err := k.get(ctx, "deployments", resourceName(name))
		if err != nil {
			return err
		}
		meta, _ := d["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if meta["deletionTimestamp"] != nil || labels["app.kubernetes.io/managed-by"] != "herdr-hub" {
			return fmt.Errorf("runtime deleting or unmanaged")
		}
		spec, _ := d["spec"].(map[string]any)
		strategy, _ := spec["strategy"].(map[string]any)
		if strategy["type"] != "Recreate" {
			return fmt.Errorf("runtime must use Recreate")
		}
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
		cs, _ := pod["containers"].([]any)
		found := false
		for _, raw := range cs {
			c, _ := raw.(map[string]any)
			found = found || c["name"] == "herdr"
		}
		if !found {
			return fmt.Errorf("herdr container missing")
		}
		addSharedMounts(pod, names)
		delete(d, "status")
		_, status, err := k.call(ctx, "PUT", "deployments", resourceName(name), d)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("runtime changed concurrently; retry")
}
func (k *kubernetes) sharedPods(ctx context.Context) ([]any, error) {
	list, err := k.get(ctx, "pods", "")
	if err != nil {
		return nil, err
	}
	pods, ok := list["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid pod list")
	}
	return pods, nil
}
func podUsesClaim(pod object, claim string) bool {
	spec, _ := pod["spec"].(map[string]any)
	volumes, _ := spec["volumes"].([]any)
	for _, raw := range volumes {
		v, _ := raw.(map[string]any)
		pvc, _ := v["persistentVolumeClaim"].(map[string]any)
		if pvc["claimName"] == claim {
			return true
		}
	}
	return false
}
func (k *kubernetes) workspaceHasSharedPods(ctx context.Context, name string) (bool, error) {
	pods, err := k.sharedPods(ctx)
	if err != nil {
		return false, err
	}
	for _, raw := range pods {
		pod, _ := raw.(map[string]any)
		meta, _ := pod["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if labels["herdr-hub/workspace"] != name {
			continue
		}
		spec, _ := pod["spec"].(map[string]any)
		volumes, _ := spec["volumes"].([]any)
		for _, raw := range volumes {
			v, _ := raw.(map[string]any)
			pvc, _ := v["persistentVolumeClaim"].(map[string]any)
			claim, _ := pvc["claimName"].(string)
			if strings.HasPrefix(claim, "herdr-shared-") {
				return true, nil
			}
		}
	}
	return false, nil
}
