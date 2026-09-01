package billing

import (
	"github.com/gsoultan/storm"

	"example.com/basic/shared"
)

// Invoice lives in a SECOND package, so the bootstrap has to sit above both it
// and model/ — which is what ShimDir computes.
type Invoice struct {
	storm.Model
	shared.Base

	Amount int64
}
