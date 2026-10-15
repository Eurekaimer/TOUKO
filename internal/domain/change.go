package domain

type ChangeKind string

const (
	Add    ChangeKind = "add"
	Update ChangeKind = "update"
	Delete ChangeKind = "delete"
)

type Change struct {
	Kind    ChangeKind `json:"kind"`
	AssetID AssetID    `json:"asset_id"`
	Asset   *Asset     `json:"asset"`
}
type ChangeSet struct {
	BaseHead RepositoryHead `json:"base_head"`
	Changes  []Change       `json:"changes"`
}
