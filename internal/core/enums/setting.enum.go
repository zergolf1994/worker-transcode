package enums

// ─── Setting Keys ────────────────────────────────────────────────────

const (
	// transcode_config = {enabled, slotRate, gpuEnabled} — shared with the
	// vdohide-service enqueuer; worker reads .enabled (kill switch) and
	// .gpuEnabled (อนุญาตใช้ GPU encoder — default true, auto-detect)
	SettingTranscodeConfig = "transcode_config"
	SettingDomainPlaylist  = "domain_playlist"
	SettingDomainProfiles  = "domain_profiles"
	SettingDomainBindings  = "domain_bindings"
)
