// Package provider describes the infrastructure operations needed by the lifecycle.
package provider

import "context"

type Machine struct {
	ID         string
	Name       string
	Owner      string
	ConfigHash string
	State      string
	Addresses  []string
	Image      string
}

type Request struct {
	Name        string
	Owner       string
	ConfigHash  string
	UserData    string
	NetworkData string
	MACAddress  string
}

// Provider operations never install or configure Kubernetes.
type Provider interface {
	Validate(context.Context) error
	Inspect(context.Context, string) (*Machine, error) // nil means absent
	Check(context.Context, Request) error              // reject unsupported infrastructure drift
	Create(context.Context, Request) error
	Start(context.Context, string) error
	Delete(context.Context, string) error
}
