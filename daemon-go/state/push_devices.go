package state

import (
	"context"
	"fmt"
	"time"
)

// PushDevice is a native app install that receives APNs pushes through the
// relay. Environment is "sandbox" (development builds) or "production".
type PushDevice struct {
	Token        string `json:"token"`
	Environment  string `json:"environment"`
	Name         string `json:"name"`
	RegisteredAt string `json:"registered_at"`
}

// UpsertPushDevice records a device token, refreshing its metadata when the
// app re-registers on launch.
func (s *Store) UpsertPushDevice(ctx context.Context, device PushDevice) error {
	if device.RegisteredAt == "" {
		device.RegisteredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_devices(token, environment, name, registered_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET environment = excluded.environment, name = excluded.name, registered_at = excluded.registered_at`,
		device.Token, device.Environment, device.Name, device.RegisteredAt)
	if err != nil {
		return fmt.Errorf("upsert push device: %w", err)
	}
	return nil
}

// DeletePushDevices removes tokens, returning how many existed.
func (s *Store) DeletePushDevices(ctx context.Context, tokens ...string) (int, error) {
	removed := 0
	for _, token := range tokens {
		result, err := s.db.ExecContext(ctx, `DELETE FROM push_devices WHERE token = ?`, token)
		if err != nil {
			return removed, fmt.Errorf("delete push device: %w", err)
		}
		n, _ := result.RowsAffected()
		removed += int(n)
	}
	return removed, nil
}

// ListPushDevices returns every registered device, newest first.
func (s *Store) ListPushDevices(ctx context.Context) ([]PushDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token, environment, name, registered_at FROM push_devices ORDER BY registered_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list push devices: %w", err)
	}
	defer rows.Close()
	devices := []PushDevice{}
	for rows.Next() {
		var device PushDevice
		if err := rows.Scan(&device.Token, &device.Environment, &device.Name, &device.RegisteredAt); err != nil {
			return nil, fmt.Errorf("scan push device: %w", err)
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}
