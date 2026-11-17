package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
)

type workspaceResources struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

func legacyWorkspaceResources() workspaceResources {
	return workspaceResources{CPURequest: "500m", MemoryRequest: "1Gi", MemoryLimit: "4Gi"}
}

// Accept common Kubernetes quantities without bringing in the Kubernetes SDK.
// CPU is expressed in cores or whole millicores; memory in bytes or binary units.
func resourceQuantity(value string, cpu bool) (*big.Rat, error) {
	if len(value) > 32 {
		return nil, fmt.Errorf("resource quantity is too large")
	}
	pattern := `^[0-9]+(Ki|Mi|Gi|Ti)?$`
	if cpu {
		pattern = `^([0-9]+m|[0-9]+(\.[0-9]{1,3})?)$`
	}
	if !regexp.MustCompile(pattern).MatchString(value) {
		return nil, fmt.Errorf("use CPU cores or millicores (500m, 2), and memory bytes or Ki/Mi/Gi/Ti (1Gi)")
	}
	factor := new(big.Rat).SetInt64(1)
	if cpu && strings.HasSuffix(value, "m") {
		value = strings.TrimSuffix(value, "m")
		factor.SetFrac64(1, 1000)
	} else if !cpu {
		for i, suffix := range []string{"Ki", "Mi", "Gi", "Ti"} {
			if strings.HasSuffix(value, suffix) {
				value = strings.TrimSuffix(value, suffix)
				factor.SetInt(new(big.Int).Lsh(big.NewInt(1), uint(10*(i+1))))
				break
			}
		}
	}
	// Force decimal parsing: integer SetString otherwise treats leading zeros as octal.
	if !strings.Contains(value, ".") {
		value += ".0"
	}
	n, ok := new(big.Rat).SetString(value)
	if !ok || n.Sign() <= 0 {
		return nil, fmt.Errorf("resource quantities must be positive")
	}
	n.Mul(n, factor)
	if n.Cmp(new(big.Rat).SetInt(new(big.Int).SetUint64(1<<63-1))) > 0 {
		return nil, fmt.Errorf("resource quantity is too large")
	}
	return n, nil
}

func (v workspaceResources) validate() error {
	for _, pair := range []struct {
		name, request, limit string
		cpu                  bool
	}{
		{"CPU", v.CPURequest, v.CPULimit, true},
		{"memory", v.MemoryRequest, v.MemoryLimit, false},
	} {
		request, err := resourceQuantity(pair.request, pair.cpu)
		if err != nil {
			return fmt.Errorf("%s request: %w", pair.name, err)
		}
		if pair.cpu && pair.limit == "" {
			continue
		} // Optional CPU cap, matching legacy workspaces.
		limit, err := resourceQuantity(pair.limit, pair.cpu)
		if err != nil {
			return fmt.Errorf("%s limit: %w", pair.name, err)
		}
		if request.Cmp(limit) > 0 {
			return fmt.Errorf("%s request must not exceed its limit", pair.name)
		}
	}
	return nil
}

func (v workspaceResources) kubernetesResources() object {
	if v.CPURequest == "" {
		v = legacyWorkspaceResources()
	}
	limits := object{"memory": v.MemoryLimit}
	if v.CPULimit != "" {
		limits["cpu"] = v.CPULimit
	}
	return object{"requests": object{"cpu": v.CPURequest, "memory": v.MemoryRequest}, "limits": limits}
}

func (a *application) resourceDefaults(ctx context.Context) (workspaceResources, error) {
	var defaults workspaceResources
	err := a.db.QueryRow(ctx, `SELECT resources FROM hub_resource_defaults WHERE id=1`).Scan(&defaults)
	return defaults, err
}

func (a *application) config(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PATCH" {
		var input struct {
			Resources workspaceResources `json:"resource_defaults"`
			Limits    *securityLimits    `json:"security_limits"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, 400, "invalid resource defaults JSON")
			return
		}
		if err := input.Resources.validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		limits, err := a.securityLimits(r.Context())
		if err != nil {
			writeError(w, 500, "could not load security limits")
			return
		}
		if input.Limits != nil {
			limits = *input.Limits
		}
		if err = limits.validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err = limits.checkResources(input.Resources); err != nil {
			writeError(w, 400, "defaults: "+err.Error())
			return
		}
		if _, err := a.db.Exec(r.Context(), `UPDATE hub_resource_defaults SET resources=$1,security_limits=$2 WHERE id=1`, input.Resources, limits); err != nil {
			writeError(w, 500, "could not save resource defaults")
			return
		}
	}
	defaults, err := a.resourceDefaults(r.Context())
	if err != nil {
		writeError(w, 500, "could not load resource defaults")
		return
	}
	limits, err := a.securityLimits(r.Context())
	if err != nil {
		writeError(w, 500, "could not load security limits")
		return
	}
	writeJSON(w, 200, map[string]any{"domain": a.kube.domain, "ssh_host": a.kube.sshHost, "storage_size": a.kube.storageSize, "storage_class": a.kube.storageClass, "resource_defaults": defaults, "security_limits": limits})
}
