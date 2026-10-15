package repository

import "github.com/Eurekaimer/TOUKO/internal/domain"

const ManifestPath = ".touko/repository.json"

func AssetPath(id domain.AssetID) string { return ".touko/assets/" + id.String() + ".json" }
func BuildObjectPath(namespace domain.Namespace, object domain.ContentObject) string {
	h := object.Hash.String()
	return "objects/" + string(namespace) + "/sha256/" + h[:2] + "/" + h[2:4] + "/" + h + "." + object.Ext
}
