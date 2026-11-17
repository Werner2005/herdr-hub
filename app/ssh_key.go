package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Recover whitespace-wrapped key data, retaining the separators around the
// base64 payload. Store one canonical authorized_keys entry per line.
func normalizePublicKeys(input string) (string, error) {
	if strings.Contains(input, "-----BEGIN") || strings.Contains(input, "-----END") {
		return "", fmt.Errorf("provide OpenSSH public keys, not private keys")
	}
	fields := strings.Fields(input)
	keys := []string{}
	seen := map[string]bool{}
	keyType := func(s string) bool {
		switch s {
		case ssh.KeyAlgoRSA, ssh.KeyAlgoDSA, ssh.KeyAlgoED25519,
			ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256,
			ssh.CertAlgoRSAv01, ssh.CertAlgoDSAv01, ssh.CertAlgoED25519v01,
			ssh.CertAlgoECDSA256v01, ssh.CertAlgoECDSA384v01, ssh.CertAlgoECDSA521v01,
			ssh.CertAlgoSKED25519v01, ssh.CertAlgoSKECDSA256v01:
			return true
		}
		return false
	}
	for len(fields) > 0 {
		typ := fields[0]
		if !keyType(typ) {
			return "", fmt.Errorf("provide OpenSSH public keys, not private keys or authorized_keys options")
		}
		fields = fields[1:]
		blob := ""
		var canonical string
		for len(fields) > 0 && !keyType(fields[0]) {
			blob += fields[0]
			fields = fields[1:]
			key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(typ + " " + blob))
			if err == nil && key.Type() == typ && len(options) == 0 && len(rest) == 0 {
				canonical = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
				break
			}
		}
		if canonical == "" {
			return "", fmt.Errorf("provide valid OpenSSH public keys")
		}
		comment := []string{}
		for len(fields) > 0 && !keyType(fields[0]) {
			comment = append(comment, fields[0])
			fields = fields[1:]
		}
		if !seen[canonical] {
			seen[canonical] = true
			if len(comment) > 0 {
				canonical += " " + strings.Join(comment, " ")
			}
			keys = append(keys, canonical)
		}
	}
	return strings.Join(keys, "\n"), nil
}

func (a *application) updateSSHKey(w http.ResponseWriter, r *http.Request, name string) {
	var input struct {
		Key *string `json:"ssh_public_key"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Key == nil {
		writeError(w, 400, "invalid SSH key JSON")
		return
	}
	key, err := normalizePublicKeys(*input.Key)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	item, err := a.load(r.Context(), name)
	if err != nil {
		writeError(w, 404, "workspace not found")
		return
	}
	if item.Status == "deleting" || item.Status == "undeployed" || item.Status == "undeploying" {
		writeError(w, 409, "deploy the workspace before updating SSH keys")
		return
	}
	if err = a.kube.updateSSHKey(r.Context(), name, key); err != nil {
		writeError(w, 502, "SSH key update incomplete; retry: "+err.Error())
		return
	}
	if _, err = a.db.Exec(r.Context(), `UPDATE workspaces SET ssh_public_key=$2 WHERE name=$1`, name, key); err != nil {
		writeError(w, 500, "SSH key applied; retry to save database key")
		return
	}
	item.SSHKey = key
	writeJSON(w, 200, item)
}

// The subPath Secret mount only refreshes when the pod is recreated. A stable
// key fingerprint makes retries idempotent while preserving the deployment spec.
func (k *kubernetes) updateSSHKey(ctx context.Context, name, key string) error {
	name = resourceName(name)
	for attempt := 0; attempt < 5; attempt++ {
		secret, err := k.get(ctx, "secrets", name)
		if err != nil {
			return err
		}
		deployment, err := k.get(ctx, "deployments", name)
		if err != nil {
			return err
		}
		for _, obj := range []object{secret, deployment} {
			meta, _ := obj["metadata"].(map[string]any)
			labels, _ := meta["labels"].(map[string]any)
			if labels["app.kubernetes.io/managed-by"] != "herdr-hub" || meta["deletionTimestamp"] != nil {
				return fmt.Errorf("resource %s is unmanaged or deleting", name)
			}
		}
		data, ok := secret["data"].(map[string]any)
		if !ok {
			return fmt.Errorf("workspace Secret data missing")
		}
		spec, _ := deployment["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		meta, ok := template["metadata"].(map[string]any)
		if !ok {
			return fmt.Errorf("workspace pod template missing")
		}
		annotations, _ := meta["annotations"].(map[string]any)
		if annotations == nil {
			annotations = object{}
			meta["annotations"] = annotations
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(key + "\n"))
		if data["authorized_keys"] != encoded {
			data["authorized_keys"] = encoded
			delete(secret, "status")
			_, status, err := k.call(ctx, "PUT", "secrets", name, secret)
			if status == 409 {
				continue
			}
			if err != nil {
				return err
			}
		}
		fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		if annotations["herdr-hub/ssh-key-hash"] == fingerprint {
			return nil
		}
		annotations["herdr-hub/ssh-key-hash"] = fingerprint
		delete(deployment, "status")
		_, status, err := k.call(ctx, "PUT", "deployments", name, deployment)
		if status == 409 {
			continue
		}
		return err
	}
	return fmt.Errorf("workspace changed concurrently; retry")
}
