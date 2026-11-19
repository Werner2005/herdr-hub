package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"sort"
	"time"
)

func childResource(id string) string { return "herdr-wl-" + id }

func childLabels(id, parent string) object {
	// Do not use herdr-hub/workspace here: that label would make parent Services
	// and ReplicaSets select these children as if they were editor pods.
	return object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/child-id": id, "herdr-hub/child-parent": parent}
}

func childObject(kind, id, parent string, spec object) object {
	api := "v1"
	if kind == "NetworkPolicy" {
		api = "networking.k8s.io/v1"
	}
	return object{"apiVersion": api, "kind": kind, "metadata": object{"name": childResource(id), "labels": childLabels(id, parent)}, "spec": spec}
}

func childEnvSecret(c childRecord, values map[string]string) object {
	obj := childObject("Secret", c.ID, c.Parent, nil)
	delete(obj, "spec")
	data := object{}
	for k, v := range values {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	obj["data"], obj["type"], obj["immutable"] = data, "Opaque", true
	return obj
}

func ownedParentPVC(obj object, parent string) bool {
	meta, _ := obj["metadata"].(object)
	labels, _ := meta["labels"].(object)
	return meta["deletionTimestamp"] == nil && labels["app.kubernetes.io/managed-by"] == "herdr-hub" && labels["herdr-hub/workspace"] == parent
}

func childOwned(obj object, id, parent string) bool {
	meta, _ := obj["metadata"].(object)
	labels, _ := meta["labels"].(object)
	return labels["app.kubernetes.io/managed-by"] == "herdr-hub" && labels["herdr-hub/child-id"] == id && labels["herdr-hub/child-parent"] == parent
}

var childKinds = map[string]string{"Pod": "pods", "Secret": "secrets", "Service": "services", "NetworkPolicy": "networkpolicies", "PersistentVolumeClaim": "persistentvolumeclaims"}

// Create-only. Never adopt another managed resource merely because its name
// matches, and never overwrite unrelated metadata or user changes.
func (k *kubernetes) ensureChild(ctx context.Context, desired object) error {
	kind := childKinds[desired["kind"].(string)]
	meta := desired["metadata"].(object)
	labels := meta["labels"].(object)
	name := meta["name"].(string)
	current, status, err := k.call(ctx, "GET", kind, name, nil)
	if status == 404 {
		_, _, err = k.call(ctx, "POST", kind, "", desired)
		return err
	}
	if err != nil {
		return err
	}
	existing, _ := current["metadata"].(object)
	if !childOwned(current, labels["herdr-hub/child-id"].(string), labels["herdr-hub/child-parent"].(string)) || existing["deletionTimestamp"] != nil {
		return fmt.Errorf("child resource is unmanaged, foreign or deleting")
	}
	return nil
}

// Return success only after confirmed absence. UID preconditions protect a
// replacement object between GET and DELETE. Reservations survive HTTP failures.
func (k *kubernetes) deleteChildObject(ctx context.Context, kind, name, id, parent string) error {
	obj, status, err := k.call(ctx, "GET", kind, name, nil)
	if status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if !childOwned(obj, id, parent) {
		return fmt.Errorf("refusing to delete foreign child resource")
	}
	meta, _ := obj["metadata"].(object)
	uid, _ := meta["uid"].(string)
	if uid == "" {
		return fmt.Errorf("child resource UID is unavailable")
	}
	if _, _, err = k.call(ctx, "DELETE", kind, name, object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": uid}}); err != nil {
		return err
	}
	if _, status, err = k.call(ctx, "GET", kind, name, nil); status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("child resource is still deleting")
}

func (k *kubernetes) childPVC(v childVolume) object {
	spec := object{"accessModes": []string{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": v.Size}}}
	if k.storageClass != "" {
		spec["storageClassName"] = k.storageClass
	}
	return childObject("PersistentVolumeClaim", v.ID, v.Parent, spec)
}

func (k *kubernetes) childPod(c childRecord) object {
	s := c.Spec
	remaining := int64(time.Until(c.ExpiresAt).Seconds())
	if remaining < 1 {
		remaining = 1
	}
	mounts, volumes := []object{}, []object{}
	for i, m := range c.Mounts {
		name := fmt.Sprintf("data-%d", i)
		claim := childResource(m.VolumeID)
		if m.Home {
			claim = resourceName(c.Parent)
		}
		volumes = append(volumes, object{"name": name, "persistentVolumeClaim": object{"claimName": claim, "readOnly": m.ReadOnly}})
		vm := object{"name": name, "mountPath": m.Target, "readOnly": m.ReadOnly}
		if m.Home && m.SubPath != "." {
			vm["subPath"] = m.SubPath
		}
		mounts = append(mounts, vm)
	}
	container := object{
		"name": "workload", "image": s.Image, "imagePullPolicy": "Always", "volumeMounts": mounts,
		"envFrom":         []object{{"secretRef": object{"name": childResource(c.ID)}}},
		"securityContext": object{"runAsNonRoot": true, "runAsUser": 1001, "runAsGroup": 1001, "allowPrivilegeEscalation": false, "capabilities": object{"drop": []string{"ALL"}}},
		"resources":       object{"requests": object{"cpu": s.CPU, "memory": s.Memory, "ephemeral-storage": "1Gi"}, "limits": object{"cpu": s.CPU, "memory": s.Memory, "ephemeral-storage": "1Gi"}},
	}
	// Kubernetes command replaces ENTRYPOINT; args replaces CMD. With no
	// explicit entrypoint a supplied command overrides the image CMD only.
	if len(s.Entrypoint) > 0 {
		container["command"] = s.Entrypoint
	}
	if len(s.Command) > 0 {
		container["args"] = s.Command
	}
	if s.Workdir != "" {
		container["workingDir"] = s.Workdir
	}
	pod := object{
		"automountServiceAccountToken": false, "enableServiceLinks": false, "restartPolicy": "Never",
		"securityContext": object{"fsGroup": 1001, "fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": object{"type": "RuntimeDefault"}},
		"containers":      []object{container}, "volumes": volumes,
		"affinity": object{"podAffinity": object{"requiredDuringSchedulingIgnoredDuringExecution": []object{{
			"labelSelector": object{"matchLabels": object{"herdr-hub/workspace": c.Parent, "app.kubernetes.io/managed-by": "herdr-hub"}},
			"topologyKey":   "kubernetes.io/hostname",
		}}}},
		"activeDeadlineSeconds": remaining,
	}
	if runtimeClass := os.Getenv("WORKSPACE_RUNTIME_CLASS"); runtimeClass != "" {
		pod["runtimeClassName"] = runtimeClass
	}
	return childObject("Pod", c.ID, c.Parent, pod)
}

// Each child has a persistent deny baseline even if the chart feature is later
// disabled. Static Helm egress adds DNS/public Internet, never private exceptions.
func (k *kubernetes) childNetwork(c childRecord) []object {
	selector := object{"matchLabels": childLabels(c.ID, c.Parent)}
	parentPeer := object{"podSelector": object{"matchLabels": object{"herdr-hub/workspace": c.Parent, "app.kubernetes.io/managed-by": "herdr-hub"}}}
	childPeer := object{"podSelector": object{"matchLabels": object{"herdr-hub/child-parent": c.Parent, "app.kubernetes.io/managed-by": "herdr-hub"}}}
	ports, servicePorts := []object{}, []object{}
	for _, n := range c.Spec.Ports {
		ports = append(ports, object{"protocol": "TCP", "port": n})
		servicePorts = append(servicePorts, object{"name": fmt.Sprintf("tcp-%d", n), "protocol": "TCP", "port": n, "targetPort": n})
	}
	ingress := []object{}
	if len(ports) > 0 {
		ingress = append(ingress, object{"from": []object{parentPeer, childPeer}, "ports": ports})
	}
	policy := childObject("NetworkPolicy", c.ID, c.Parent, object{
		"podSelector": selector, "policyTypes": []string{"Ingress", "Egress"},
		"ingress": ingress, "egress": []object{{"to": []object{parentPeer, childPeer}}},
	})
	// The parent explicitly permits this child's connections. Other groups
	// cannot match the immutable child ID/parent labels.
	back := childObject("NetworkPolicy", c.ID, c.Parent, object{
		"podSelector": object{"matchLabels": object{"herdr-hub/workspace": c.Parent, "app.kubernetes.io/managed-by": "herdr-hub"}},
		"policyTypes": []string{"Ingress", "Egress"},
		"ingress":     []object{{"from": []object{{"podSelector": selector}}}},
		"egress":      childEgressTo(selector, ports),
	})
	back["metadata"].(object)["name"] = childResource(c.ID) + "-parent"
	result := []object{policy, back}
	if len(servicePorts) > 0 {
		result = append(result, childObject("Service", c.ID, c.Parent, object{"selector": childLabels(c.ID, c.Parent), "ports": servicePorts}))
	}
	return result
}

func childEgressTo(selector object, ports []object) []object {
	if len(ports) == 0 {
		return []object{}
	}
	return []object{{"to": []object{{"podSelector": selector}}, "ports": ports}}
}

func (a *application) syncChildVolume(ctx context.Context, v childVolume) error {
	if v.Deleting {
		if err := a.kube.deleteChildObject(ctx, "persistentvolumeclaims", childResource(v.ID), v.ID, v.Parent); err != nil {
			return err
		}
		_, err := a.db.Exec(ctx, `DELETE FROM hub_child_volumes WHERE id=$1`, v.ID)
		return err
	}
	if v.Provisioned {
		pvc, _, err := a.kube.call(ctx, "GET", "persistentvolumeclaims", childResource(v.ID), nil)
		if err != nil || !childOwned(pvc, v.ID, v.Parent) {
			return fmt.Errorf("previously provisioned child PVC is missing or foreign; refusing to recreate data")
		}
		return nil
	}
	if err := a.kube.ensureChild(ctx, a.kube.childPVC(v)); err != nil {
		return err
	}
	_, err := a.db.Exec(ctx, `UPDATE hub_child_volumes SET provisioned=true WHERE id=$1`, v.ID)
	return err
}

func (a *application) childVolumes(ctx context.Context, parent string) ([]childVolume, error) {
	rows, err := a.db.Query(ctx, `SELECT id,parent,name,size,anonymous_for,deleting,provisioned FROM hub_child_volumes WHERE parent=$1 ORDER BY name`, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []childVolume{}
	for rows.Next() {
		var v childVolume
		if err := rows.Scan(&v.ID, &v.Parent, &v.Name, &v.Size, &v.AnonymousFor, &v.Deleting, &v.Provisioned); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (a *application) syncChild(ctx context.Context, c childRecord) error {
	name := childResource(c.ID)
	if c.Status == "deleting" {
		if err := a.kube.deleteChildObject(ctx, "pods", name, c.ID, c.Parent); err != nil {
			return err
		}
		for _, entry := range []struct{ kind, name string }{{"services", name}, {"networkpolicies", name}, {"networkpolicies", name + "-parent"}, {"secrets", name}} {
			if err := a.kube.deleteChildObject(ctx, entry.kind, entry.name, c.ID, c.Parent); err != nil {
				return err
			}
		}
		volumes, err := a.childVolumes(ctx, c.Parent)
		if err != nil {
			return err
		}
		for _, v := range volumes {
			if v.AnonymousFor == c.ID {
				if _, err = a.db.Exec(ctx, `UPDATE hub_child_volumes SET deleting=true WHERE id=$1`, v.ID); err != nil {
					return err
				}
				v.Deleting = true
				if err = a.syncChildVolume(ctx, v); err != nil {
					return err
				}
			}
		}
		_, err = a.db.Exec(ctx, `DELETE FROM hub_children WHERE id=$1`, c.ID)
		return err
	}
	if time.Now().After(c.ExpiresAt) || ((c.Status == "succeeded" || c.Status == "failed") && c.Spec.AutoRemove) {
		if _, err := a.db.Exec(ctx, `UPDATE hub_children SET status='deleting' WHERE id=$1`, c.ID); err != nil {
			return err
		}
		c.Status = "deleting"
		return a.syncChild(ctx, c)
	}
	if c.Status == "preparing" {
		secret, _, err := a.kube.call(ctx, "GET", "secrets", name, nil)
		if err != nil || !childOwned(secret, c.ID, c.Parent) {
			_, e := a.db.Exec(ctx, `UPDATE hub_children SET status='error',error='environment Secret unavailable; remove and recreate workload' WHERE id=$1`, c.ID)
			return e
		}
		if _, err = a.db.Exec(ctx, `UPDATE hub_children SET status='pending' WHERE id=$1`, c.ID); err != nil {
			return err
		}
		c.Status = "pending"
	}
	if c.Status == "error" || c.Status == "lost" {
		return nil
	}
	if c.Status == "pending" {
		if !workloadsEnabled() {
			return nil
		}
		var permitted bool
		if err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hub_child_access b JOIN workspaces w ON w.name=b.parent
 WHERE b.parent=$1 AND (b.budget->>'enabled')::boolean AND w.status NOT IN ('deleting','undeploying','undeployed'))`, c.Parent).Scan(&permitted); err != nil {
			return err
		}
		if !permitted {
			return nil
		}
		volumes, err := a.childVolumes(ctx, c.Parent)
		if err != nil {
			return err
		}
		byID := map[string]childVolume{}
		for _, v := range volumes {
			byID[v.ID] = v
		}
		ids := []string{}
		for _, m := range c.Mounts {
			if !m.Home {
				ids = append(ids, m.VolumeID)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			v, ok := byID[id]
			if !ok || v.Deleting {
				return fmt.Errorf("reserved volume unavailable")
			}
			if err := a.syncChildVolume(ctx, v); err != nil {
				return err
			}
		}
		for _, obj := range a.kube.childNetwork(c) {
			if err := a.kube.ensureChild(ctx, obj); err != nil {
				return err
			}
		}
		// Commit launch intent BEFORE submitting the Pod. A crash or ambiguous
		// API failure must never recreate a previously executed command.
		if _, err := a.db.Exec(ctx, `UPDATE hub_children SET status='launching' WHERE id=$1`, c.ID); err != nil {
			return err
		}
		if err := a.kube.ensureChild(ctx, a.kube.childPod(c)); err != nil {
			return err
		}
	}
	pod, status, err := a.kube.call(ctx, "GET", "pods", name, nil)
	if status == 404 {
		_, err = a.db.Exec(ctx, `UPDATE hub_children SET status='lost',error='pod missing; execution is not retried automatically' WHERE id=$1`, c.ID)
		return err
	}
	if err != nil {
		return err
	}
	if !childOwned(pod, c.ID, c.Parent) {
		return fmt.Errorf("foreign child Pod")
	}
	ps, _ := pod["status"].(object)
	phase, _ := ps["phase"].(string)
	state := "starting"
	switch phase {
	case "Running":
		state = "running"
	case "Succeeded":
		state = "succeeded"
	case "Failed":
		state = "failed"
	}
	var exit *int
	if state == "succeeded" || state == "failed" {
		for _, raw := range objects(ps["containerStatuses"]) {
			if raw["name"] == "workload" {
				s, _ := raw["state"].(object)
				terminated, _ := s["terminated"].(object)
				if n, ok := terminated["exitCode"].(float64); ok {
					value := int(n)
					exit = &value
				}
			}
		}
	}
	_, err = a.db.Exec(ctx, `UPDATE hub_children SET status=$2,exit_code=$3,error='' WHERE id=$1`, c.ID, state, exit)
	return err
}

func (a *application) reconcileChildren(ctx context.Context) error {
	rows, err := a.db.Query(ctx, `SELECT parent,name FROM hub_children ORDER BY id`)
	if err != nil {
		return err
	}
	type key struct{ parent, name string }
	keys := []key{}
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.parent, &k.name); err != nil {
			break
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return fmt.Errorf("could not load child reconciliation state")
	}
	var first error
	for _, key := range keys {
		item, e := a.loadChild(ctx, key.parent, key.name)
		if e == nil {
			e = a.syncChild(ctx, item)
		}
		if e != nil {
			// Store a bounded generic error, not raw responses or env contents.
			_, _ = a.db.Exec(ctx, `UPDATE hub_children SET error='Kubernetes reconciliation pending' WHERE parent=$1 AND name=$2`, key.parent, key.name)
			if first == nil {
				first = e
			}
		}
	}
	rows, err = a.db.Query(ctx, `SELECT DISTINCT parent FROM hub_child_volumes`)
	if err != nil {
		return err
	}
	parents := []string{}
	for rows.Next() {
		var parent string
		if err = rows.Scan(&parent); err != nil {
			break
		}
		parents = append(parents, parent)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return fmt.Errorf("could not load volume reconciliation state")
	}
	for _, parent := range parents {
		volumes, e := a.childVolumes(ctx, parent)
		if e != nil {
			return e
		}
		for _, v := range volumes {
			if !v.Deleting && (!workloadsEnabled() || v.AnonymousFor != "") {
				continue
			}
			if e = a.syncChildVolume(ctx, v); e != nil && first == nil {
				first = e
			}
		}
	}
	return first
}

func (a *application) runChildUpdates(ctx context.Context) {
	for {
		cycle, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := a.mu.lock(cycle); err == nil {
			err = a.reconcileChildren(cycle)
			a.mu.unlock()
			if err != nil {
				log.Print("child workload reconciliation pending")
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}
