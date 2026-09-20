package constants

type BalanceStatus string

const (
	BalanceQueued        BalanceStatus = "queued"
	BalanceCalculating   BalanceStatus = "calculating"
	BalancePendingReview BalanceStatus = "pending_review"
	BalanceAccepted      BalanceStatus = "accepted"
	BalanceRejected      BalanceStatus = "rejected"
	BalanceInvalidated   BalanceStatus = "invalidated"
)

var BalanceStatuses = []BalanceStatus{
	BalanceQueued, BalanceCalculating, BalancePendingReview,
	BalanceAccepted, BalanceRejected, BalanceInvalidated,
}

func ValidBalanceStatus(value BalanceStatus) bool {
	for _, status := range BalanceStatuses {
		if status == value {
			return true
		}
	}
	return false
}

func CanTransitionBalance(from, to BalanceStatus) bool {
	switch from {
	case BalanceQueued:
		return to == BalanceCalculating || to == BalanceInvalidated
	case BalanceCalculating:
		return to == BalancePendingReview || to == BalanceInvalidated
	case BalancePendingReview:
		return to == BalanceAccepted || to == BalanceRejected || to == BalanceInvalidated
	case BalanceRejected:
		return to == BalanceInvalidated
	default:
		return false
	}
}

// EvidenceGuardedStatuses 是仍受证据冻结校验约束的非终态状态：
// 期间证据变化时这些运行会被标记过期，且过期后不得提交或复核。
// accepted 与 invalidated 为终态，冻结清单只作不可变历史保留。
var EvidenceGuardedStatuses = []BalanceStatus{
	BalanceCalculating,
	BalancePendingReview,
	BalanceRejected,
}

// EvidenceFreezeGuarded 判断指定状态是否参与证据过期标记与提交/复核拦截。
func EvidenceFreezeGuarded(status BalanceStatus) bool {
	for _, candidate := range EvidenceGuardedStatuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func BalanceStatusValues() []string {
	values := make([]string, 0, len(BalanceStatuses))
	for _, status := range BalanceStatuses {
		values = append(values, string(status))
	}
	return values
}
