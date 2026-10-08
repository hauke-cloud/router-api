package contract

import (
	"k8s.io/apimachinery/pkg/api/equality"
)

func equalJSON(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return equality.Semantic.DeepEqual(a, b)
}
