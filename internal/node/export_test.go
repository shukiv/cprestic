package node

import (
	"time"

	"github.com/shuki/cprest/internal/nodestore"
)

// RetentionIsThrottledForTest reports whether this repository is inside
// the window that keeps retention from taking the repository lock again.
func (e *Engine) RetentionIsThrottledForTest(repo nodestore.Repository) bool {
	last := lastRetentionAttempt(repo.Retention)
	return !last.IsZero() && time.Since(last) < retentionEvery
}
