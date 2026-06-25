// Package agentcfg is the JSON contract between the Director (which serves it)
// and a managed collector (which pulls and applies it). It has no heavy
// dependencies so the collector binary stays lean.
package agentcfg

// Device is one exporter the collector should accept and how to stamp/filter it.
type Device struct {
	Name        string `json:"name"`
	ExporterIP  string `json:"exporter_ip"`
	ISPID       uint32 `json:"isp_id"`
	DeviceID    uint32 `json:"device_id"`
	Protocol    string `json:"protocol"`
	Profile     string `json:"profile"`
	Enabled     bool   `json:"enabled"`
	SkipDNS     bool   `json:"skip_dns"`
	SkipPrivate bool   `json:"skip_private_to_private"`
	SkipZero    bool   `json:"skip_zero_bytes"`
}

// Bundle is the full device-registry + policy the collector applies on pull.
type Bundle struct {
	Version             string   `json:"version"`               // content hash for change detection
	UnknownExporterMode string   `json:"unknown_exporter_mode"` // managed mode defaults to "reject"
	Devices             []Device `json:"devices"`
}
