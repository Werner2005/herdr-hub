package main

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

type workspaceUsage struct {
	CPUMillicores float64   `json:"cpu_millicores"`
	MemoryBytes   float64   `json:"memory_bytes"`
	SampledAt     time.Time `json:"sampled_at"`
}

var usageQuantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(n|u|m|Ki|Mi|Gi|Ti|Pi|Ei|k|K|M|G|T|P|E)?$`)

// Metrics use finer CPU units than settings (usually nanocores) and binary
// memory units. Preserve precision until converting the final totals for JSON.
func usageQuantity(value string) (*big.Rat, error) {
	if len(value) > 64 {
		return nil, fmt.Errorf("invalid usage quantity")
	}
	parts := usageQuantityPattern.FindStringSubmatch(value)
	if parts == nil {
		return nil, fmt.Errorf("invalid usage quantity")
	}
	number := parts[1]
	if !strings.Contains(number, ".") {
		number += ".0"
	}
	n, ok := new(big.Rat).SetString(number)
	if !ok {
		return nil, fmt.Errorf("invalid usage quantity")
	}
	factor := new(big.Rat).SetInt64(1)
	switch parts[2] {
	case "n":
		factor.SetFrac64(1, 1000000000)
	case "u":
		factor.SetFrac64(1, 1000000)
	case "m":
		factor.SetFrac64(1, 1000)
	default:
		for i, suffix := range []string{"Ki", "Mi", "Gi", "Ti", "Pi", "Ei"} {
			if parts[2] == suffix {
				factor.SetInt(new(big.Int).Lsh(big.NewInt(1), uint(10*(i+1))))
				break
			}
		}
		for i, suffix := range []string{"K", "M", "G", "T", "P", "E"} {
			if strings.ToUpper(parts[2]) == suffix {
				factor.SetInt(new(big.Int).Exp(big.NewInt(1000), big.NewInt(int64(i+1)), nil))
				break
			}
		}
	}
	n.Mul(n, factor)
	if n.Cmp(new(big.Rat).SetInt(new(big.Int).SetUint64(1<<63-1))) > 0 {
		return nil, fmt.Errorf("usage quantity too large")
	}
	return n, nil
}

// Metrics are optional: missing metrics-server/RBAC/samples must not break the
// workspace list. Join PodMetrics to owned pods, never infer ownership by name.
func (k *kubernetes) workspaceUsage(ctx context.Context, names []string) map[string]*workspaceUsage {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	visible := map[string]bool{}
	for _, name := range names {
		visible[name] = true
	}
	pods, err := k.get(ctx, "pods", "")
	if err != nil {
		return nil
	}
	podOwners := map[string]string{}
	for _, raw := range anyItems(pods) {
		pod, _ := raw.(object)
		meta, _ := pod["metadata"].(object)
		labels, _ := meta["labels"].(object)
		owner, _ := labels["herdr-hub/workspace"].(string)
		status, _ := pod["status"].(object)
		if !visible[owner] || labels["app.kubernetes.io/managed-by"] != "herdr-hub" || meta["deletionTimestamp"] != nil || status["phase"] == "Succeeded" || status["phase"] == "Failed" {
			continue
		}
		name, _ := meta["name"].(string)
		if name != "" {
			podOwners[name] = owner
		}
	}
	if len(podOwners) == 0 {
		return nil
	}
	metrics, err := k.get(ctx, "podmetrics", "")
	if err != nil {
		return nil
	}
	return aggregateWorkspaceUsage(podOwners, anyItems(metrics), time.Now())
}

func anyItems(list object) []any {
	items, _ := list["items"].([]any)
	return items
}

func aggregateWorkspaceUsage(podOwners map[string]string, metrics []any, now time.Time) map[string]*workspaceUsage {
	result := map[string]*workspaceUsage{}
	sampled := map[string]bool{}
	for _, raw := range metrics {
		metric, _ := raw.(object)
		meta, _ := metric["metadata"].(object)
		name, _ := meta["name"].(string)
		owner, ok := podOwners[name]
		if !ok || sampled[name] {
			continue
		}
		timestamp, _ := metric["timestamp"].(string)
		at, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || now.Sub(at) > 2*time.Minute || at.After(now.Add(30*time.Second)) {
			continue
		}
		containers, _ := metric["containers"].([]any)
		for _, raw := range containers {
			container, _ := raw.(object)
			if container["name"] != "herdr" {
				continue
			}
			usage, _ := container["usage"].(object)
			cpuString, _ := usage["cpu"].(string)
			memoryString, _ := usage["memory"].(string)
			cpu, cpuErr := usageQuantity(cpuString)
			memory, memoryErr := usageQuantity(memoryString)
			if cpuErr != nil || memoryErr != nil {
				break
			}
			if result[owner] == nil {
				result[owner] = &workspaceUsage{SampledAt: at}
			}
			cpu.Mul(cpu, big.NewRat(1000, 1))
			cpuValue, _ := cpu.Float64()
			memoryValue, _ := memory.Float64()
			result[owner].CPUMillicores += cpuValue
			result[owner].MemoryBytes += memoryValue
			if at.Before(result[owner].SampledAt) {
				result[owner].SampledAt = at
			}
			sampled[name] = true
			break
		}
	}
	// Partial samples are not a reliable total; report unavailable until every
	// active workspace pod has a valid sample (e.g. while starting a new pod).
	for name, owner := range podOwners {
		if !sampled[name] {
			delete(result, owner)
		}
	}
	return result
}
