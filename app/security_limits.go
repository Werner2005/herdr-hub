package main

import (
	"context"
	"fmt"
)

type securityLimits struct {
	CPU           string `json:"cpu"`
	Memory        string `json:"memory"`
	VolumeGi      int    `json:"volume_gi"`
	UserStorageGi int    `json:"user_storage_gi"`
	UserVolumes   int    `json:"user_volumes"`
}

func defaultSecurityLimits() securityLimits {
	return securityLimits{"4", "8Gi", 100, 200, 10}
}

func (v securityLimits) validate() error {
	if _, err := resourceQuantity(v.CPU, true); err != nil {
		return fmt.Errorf("maximum CPU: %w", err)
	}
	if _, err := resourceQuantity(v.Memory, false); err != nil {
		return fmt.Errorf("maximum memory: %w", err)
	}
	if v.VolumeGi < 1 || v.UserStorageGi < v.VolumeGi || v.UserStorageGi > 1048576 || v.UserVolumes < 1 || v.UserVolumes > 1000 {
		return fmt.Errorf("use positive storage budgets (total >= volume, total <= 1048576 Gi) and 1–1000 volumes per user")
	}
	return nil
}

func (v securityLimits) checkResources(r workspaceResources) error {
	for _, p := range []struct {
		value, maximum, name string
		cpu                  bool
	}{{r.CPULimit, v.CPU, "CPU", true}, {r.MemoryLimit, v.Memory, "memory", false}} {
		quantity, err := resourceQuantity(p.value, p.cpu)
		if err != nil {
			return fmt.Errorf("a finite %s limit is required", p.name)
		}
		maximum, err := resourceQuantity(p.maximum, p.cpu)
		if err != nil {
			return err
		}
		if quantity.Cmp(maximum) > 0 {
			return fmt.Errorf("%s limit exceeds administrator maximum %s", p.name, p.maximum)
		}
	}
	return nil
}

func (a *application) securityLimits(ctx context.Context) (securityLimits, error) {
	var limits securityLimits
	err := a.db.QueryRow(ctx, `SELECT security_limits FROM hub_resource_defaults WHERE id=1`).Scan(&limits)
	return limits, err
}

// Called under the API mutation lock. Include workspace homes in the owner's
// storage budget, and exclude the volume being retried or expanded.
func (a *application) checkStorageBudget(ctx context.Context, owner, name, size string) error {
	limits, err := a.securityLimits(ctx)
	if err != nil {
		return err
	}
	requested, err := storageGi(size)
	if err != nil {
		return err
	}
	if requested > limits.VolumeGi {
		return fmt.Errorf("volume exceeds administrator maximum of %d Gi", limits.VolumeGi)
	}
	rows, err := a.db.Query(ctx, `SELECT storage_size FROM shared_volumes WHERE owner=$1 AND name<>$2 UNION ALL SELECT storage_size FROM workspaces WHERE owner=$1`, owner, name)
	if err != nil {
		return err
	}
	defer rows.Close()
	total := int64(requested)
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return err
		}
		if s == "" {
			s = a.kube.storageSize
		}
		gi, e := storageGi(s)
		if e != nil {
			return e
		}
		total += int64(gi)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if total > int64(limits.UserStorageGi) {
		return fmt.Errorf("owner storage budget of %d Gi exceeded", limits.UserStorageGi)
	}
	var count int
	if err = a.db.QueryRow(ctx, `SELECT count(*) FROM shared_volumes WHERE owner=$1 AND name<>$2`, owner, name).Scan(&count); err != nil {
		return err
	}
	if count >= limits.UserVolumes {
		return fmt.Errorf("owner shared-volume maximum of %d exceeded", limits.UserVolumes)
	}
	return nil
}
