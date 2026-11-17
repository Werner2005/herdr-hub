package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type object = map[string]any
type kubernetes struct {
	client                                                                                                                *http.Client
	base, namespace, tokenFile, image, domain, gateway, gatewayNamespace, webListener, sshHost, storageClass, storageSize string
	listeners                                                                                                             map[string]int
	sshPortStart, sshPortEnd                                                                                              int
}

func validName(s string) bool {
	return len(s) <= 40 && regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`).MatchString(s)
}
func validPublicKey(s string) bool {
	keys, err := normalizePublicKeys(s)
	return err == nil && keys != ""
}
func resourceName(name string) string { return "herdr-" + name }
func newKubernetes() (*kubernetes, error) {
	ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes CA")
	}
	k := &kubernetes{client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}, base: "https://" + net.JoinHostPort(os.Getenv("KUBERNETES_SERVICE_HOST"), env("KUBERNETES_SERVICE_PORT", "443")), tokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token", namespace: os.Getenv("WORKSPACE_NAMESPACE"), image: os.Getenv("WORKSPACE_IMAGE"), domain: os.Getenv("WORKSPACE_DOMAIN"), gateway: os.Getenv("WORKSPACE_GATEWAY"), gatewayNamespace: os.Getenv("WORKSPACE_GATEWAY_NAMESPACE"), webListener: env("WORKSPACE_WEB_LISTENER", "https"), sshHost: os.Getenv("WORKSPACE_SSH_HOST"), storageClass: os.Getenv("WORKSPACE_STORAGE_CLASS"), storageSize: env("WORKSPACE_STORAGE_SIZE", "10Gi"), listeners: map[string]int{}}
	if k.namespace == "" || k.image == "" || k.domain == "" || k.gateway == "" {
		return nil, fmt.Errorf("workspace namespace, image, domain and Gateway are required")
	}
	if err = json.Unmarshal([]byte(env("WORKSPACE_SSH_LISTENERS", "{}")), &k.listeners); err != nil {
		return nil, err
	}
	ports := map[int]bool{}
	k.sshPortStart, err = strconv.Atoi(env("WORKSPACE_SSH_PORT_START", "2201"))
	if err != nil {
		return nil, err
	}
	k.sshPortEnd, err = strconv.Atoi(env("WORKSPACE_SSH_PORT_END", "2299"))
	if err != nil {
		return nil, err
	}
	if k.sshPortStart < 1 || k.sshPortEnd > 65535 || k.sshPortEnd < k.sshPortStart {
		return nil, fmt.Errorf("invalid SSH port range")
	}
	if _, _, err = exportPortRange(); err != nil {
		return nil, err
	}
	for _, key := range []string{"WORKSPACE_GATEWAY_POD_SELECTOR", "WORKSPACE_GATEWAY_NAMESPACE_SELECTOR"} {
		if raw := os.Getenv(key); raw != "" {
			var selector map[string]string
			if err := json.Unmarshal([]byte(raw), &selector); err != nil || selector == nil {
				return nil, fmt.Errorf("invalid %s", key)
			}
		}
	}
	for _, p := range k.listeners {
		if p < 1 || p > 65535 || ports[p] {
			return nil, fmt.Errorf("SSH listener ports must be unique valid TCP ports")
		}
		ports[p] = true
	}
	return k, nil
}
func (k *kubernetes) endpoint(kind, name string) string {
	prefix := "/api/v1"
	if kind == "podmetrics" {
		prefix, kind = "/apis/metrics.k8s.io/v1beta1", "pods"
	}
	if kind == "networkpolicies" {
		prefix = "/apis/networking.k8s.io/v1"
	}
	if kind == "deployments" {
		prefix = "/apis/apps/v1"
	}
	if kind == "httproutes" || kind == "gateways" {
		prefix = "/apis/gateway.networking.k8s.io/v1"
	}
	if kind == "tcproutes" || kind == "udproutes" {
		prefix = "/apis/gateway.networking.k8s.io/v1alpha2"
	}
	namespace := k.namespace
	if kind == "gateways" && k.gatewayNamespace != "" {
		namespace = k.gatewayNamespace
	}
	p := k.base + prefix + "/namespaces/" + namespace + "/" + kind
	if name != "" {
		p += "/" + name
	}
	return p
}
func (k *kubernetes) call(ctx context.Context, method, kind, name string, body any) (object, int, error) {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, k.endpoint(kind, name), bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("Kubernetes %s %s: HTTP %d", method, kind, resp.StatusCode)
	}
	var obj object
	err = json.Unmarshal(data, &obj)
	return obj, resp.StatusCode, err
}
func (k *kubernetes) get(ctx context.Context, kind, name string) (object, error) {
	obj, _, err := k.call(ctx, "GET", kind, name, nil)
	return obj, err
}
func (k *kubernetes) ensure(ctx context.Context, obj object) error {
	kind := map[string]string{"Secret": "secrets", "PersistentVolumeClaim": "persistentvolumeclaims", "Deployment": "deployments", "Service": "services", "HTTPRoute": "httproutes", "TCPRoute": "tcproutes", "UDPRoute": "udproutes", "NetworkPolicy": "networkpolicies"}[obj["kind"].(string)]
	name := obj["metadata"].(object)["name"].(string)
	existing, status, err := k.call(ctx, "GET", kind, name, nil)
	if status == 404 {
		_, _, err = k.call(ctx, "POST", kind, "", obj)
		return err
	}
	if err != nil {
		return err
	}
	meta, _ := existing["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	if meta["deletionTimestamp"] != nil {
		return fmt.Errorf("resource %s is still deleting; retry shortly", name)
	}
	if labels["app.kubernetes.io/managed-by"] != "herdr-hub" {
		return fmt.Errorf("resource %s is not owned by Herdr Hub", name)
	}
	return nil
}
func (k *kubernetes) remove(ctx context.Context, kind, name string) error {
	obj, status, err := k.call(ctx, "GET", kind, name, nil)
	if status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	meta, _ := obj["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	if labels["app.kubernetes.io/managed-by"] != "herdr-hub" {
		return fmt.Errorf("refusing to delete unmanaged resource %s", name)
	}
	_, status, err = k.call(ctx, "DELETE", kind, name, object{"propagationPolicy": "Foreground"})
	if status == 404 {
		return nil
	}
	return err
}
func (k *kubernetes) resources(w workspace, token string) []object {
	pullPolicy := env("WORKSPACE_IMAGE_PULL_POLICY", "IfNotPresent")
	name := resourceName(w.Name)
	labels := object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/workspace": w.Name}
	makeObj := func(api, kind string, spec object) object {
		return object{"apiVersion": api, "kind": kind, "metadata": object{"name": name, "namespace": k.namespace, "labels": labels}, "spec": spec}
	}
	secret := makeObj("v1", "Secret", nil)
	delete(secret, "spec")
	secret["type"] = "Opaque"
	secret["data"] = object{"authorized_keys": base64.StdEncoding.EncodeToString([]byte(w.SSHKey + "\n")), "token": base64.StdEncoding.EncodeToString([]byte(token))}
	size := w.StorageSize
	if size == "" {
		size = k.storageSize
	}
	pvcSpec := object{"accessModes": []string{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": size}}}
	if k.storageClass != "" {
		pvcSpec["storageClassName"] = k.storageClass
	}
	pod := object{
		"automountServiceAccountToken": false,
		"securityContext":              object{"fsGroup": 1001, "fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": object{"type": "RuntimeDefault"}},
		"initContainers":               []object{{"name": "home-permissions", "image": "busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662", "securityContext": object{"runAsUser": 0, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}, "add": []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}}}, "command": []string{"sh", "-c", "mkdir -p /home/dev/workspace && chown 1001:1001 /home/dev /home/dev/workspace"}, "volumeMounts": []object{{"name": "home", "mountPath": "/home/dev"}}}},
		"containers":                   []object{{"name": "herdr", "image": k.image, "imagePullPolicy": pullPolicy, "ports": []object{{"name": "web", "containerPort": 7317}, {"name": "ssh", "containerPort": 2222}}, "env": []object{{"name": "HOST", "value": "0.0.0.0"}, {"name": "PORT", "value": "7317"}, {"name": "HERDR_WEB_TOKEN", "valueFrom": object{"secretKeyRef": object{"name": name, "key": "token"}}}}, "volumeMounts": []object{{"name": "home", "mountPath": "/home/dev"}, {"name": "ssh-keys", "mountPath": "/run/ssh-keys/authorized_keys", "subPath": "authorized_keys", "readOnly": true}}, "startupProbe": object{"httpGet": object{"path": "/api/health", "port": "web"}, "periodSeconds": 5, "failureThreshold": 120}, "readinessProbe": object{"httpGet": object{"path": "/api/health", "port": "web"}, "periodSeconds": 10}, "resources": w.Resources.kubernetesResources()}},
		"volumes":                      []object{{"name": "home", "persistentVolumeClaim": object{"claimName": name}}, {"name": "ssh-keys", "secret": object{"secretName": name, "defaultMode": 288, "items": []object{{"key": "authorized_keys", "path": "authorized_keys"}}}}},
	}
	// Sudo and root sshd are intentional inside workspaces. Limit their kernel
	// capabilities without setting no-new-privileges, which would break sudo.
	// Ubuntu sshd requires AUDIT_WRITE for PTY login records; without it,
	// authentication succeeds but interactive sessions terminate before the shell.
	pod["containers"].([]object)[0]["securityContext"] = object{"runAsUser": 1001, "runAsGroup": 1001, "capabilities": object{"drop": []string{"ALL"}, "add": []string{"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "SETGID", "SETUID", "SYS_CHROOT"}}}
	if runtimeClass := os.Getenv("WORKSPACE_RUNTIME_CLASS"); runtimeClass != "" {
		pod["runtimeClassName"] = runtimeClass
	}
	addSharedMounts(pod, w.Volumes)
	addConfigurationMappings(pod, w.ResolvedConfigurations)
	deployment := makeObj("apps/v1", "Deployment", object{"replicas": 1, "strategy": object{"type": "Recreate"}, "selector": object{"matchLabels": labels}, "template": object{"metadata": object{"labels": labels}, "spec": pod}})
	if len(w.ResolvedConfigurations) > 0 {
		deployment["spec"].(object)["template"].(object)["metadata"].(object)["annotations"] = object{"herdr-hub/configuration-version": configurationRevision(w.ResolvedConfigurations)}
	}
	service := makeObj("v1", "Service", object{"selector": labels, "ports": []object{{"name": "web", "port": 7317, "targetPort": "web"}, {"name": "ssh", "port": 2222, "targetPort": "ssh"}}})
	parent := func(section string) object {
		p := object{"name": k.gateway, "sectionName": section}
		if k.gatewayNamespace != "" {
			p["namespace"] = k.gatewayNamespace
		}
		return p
	}
	route := makeObj("gateway.networking.k8s.io/v1", "HTTPRoute", object{"parentRefs": []object{parent(k.webListener)}, "hostnames": []string{w.Name + "." + k.domain}, "rules": []object{{"matches": []object{{"path": object{"type": "PathPrefix", "value": "/"}}}, "backendRefs": []object{{"name": name, "port": 7317}}}}})
	result := []object{secret, makeObj("v1", "PersistentVolumeClaim", pvcSpec), deployment, service, route}
	if w.SSHListener != "" {
		result = append(result, makeObj("gateway.networking.k8s.io/v1alpha2", "TCPRoute", object{"parentRefs": []object{parent(w.SSHListener)}, "rules": []object{{"backendRefs": []object{{"name": name, "port": 2222}}}}}))
	}
	return result
}
