// Package conditions has the few helpers every controller needs to report
// status conditions the same way.
package conditions

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Set records a condition for the given generation. It reports whether the
// condition changed.
func Set(conditions *[]metav1.Condition, generation int64, conditionType string, status metav1.ConditionStatus, reason, message string) bool {
	return meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// True records a condition as True.
func True(conditions *[]metav1.Condition, generation int64, conditionType, reason, message string) bool {
	return Set(conditions, generation, conditionType, metav1.ConditionTrue, reason, message)
}

// False records a condition as False.
func False(conditions *[]metav1.Condition, generation int64, conditionType, reason, message string) bool {
	return Set(conditions, generation, conditionType, metav1.ConditionFalse, reason, message)
}

// Mirror copies a condition observed on another object under a new type.
// A missing source becomes Unknown: not having heard is not the same as no.
func Mirror(conditions *[]metav1.Condition, generation int64, conditionType string, source *metav1.Condition, missingReason, missingMessage string) bool {
	if source == nil || source.Status == "" {
		return Set(conditions, generation, conditionType, metav1.ConditionUnknown, missingReason, missingMessage)
	}
	reason := source.Reason
	if reason == "" {
		reason = "Reported"
	}
	return Set(conditions, generation, conditionType, source.Status, reason, source.Message)
}

// IsTrue reports whether the condition is True.
func IsTrue(conditions []metav1.Condition, conditionType string) bool {
	return meta.IsStatusConditionTrue(conditions, conditionType)
}

// Get returns the condition of the given type, or nil.
func Get(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(conditions, conditionType)
}
