package service

type MountRequest struct {
	MountID             string `json:"mount_id"`
	Reference           string `json:"reference"`
	CheckDiskQuota      bool   `json:"check_disk_quota"`
	ExcludeModelWeights bool   `json:"exclude_model_weights"`
}
