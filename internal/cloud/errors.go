package cloud

import "errors"

// ErrMetricNoData marks a successful CMS response that contains no usable
// metric samples. It is different from authentication, throttling, and API
// failures, which should keep their original error details.
var ErrMetricNoData = errors.New("CMS metric data is not available yet")

func IsMetricNoDataError(err error) bool {
	return errors.Is(err, ErrMetricNoData)
}
