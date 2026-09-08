package analytics

import "strings"

// Error classes. A provisioning failure is reported as one of these and never
// as its message, which carries image references, cluster addresses and once a
// full Kubernetes API URL.
const (
	ClassTimeout   = "timeout"
	ClassImagePull = "image_pull"
	ClassQuota     = "quota"
	ClassKubeAPI   = "k8s_api"
	ClassStorage   = "storage"
	ClassOther     = "other"
	ClassNone      = ""
)

// errorClassMarkers maps a lowercase substring to a class, in the order they
// are tried. A message matching none of them is ClassOther.
var errorClassMarkers = []struct {
	marker string
	class  string
}{
	{"imagepullbackoff", ClassImagePull},
	{"errimagepull", ClassImagePull},
	{"pull access denied", ClassImagePull},
	{"manifest unknown", ClassImagePull},

	{"exceeded quota", ClassQuota},
	{"insufficient", ClassQuota},
	{"forbidden: quota", ClassQuota},
	{"out of memory", ClassQuota},

	{"timed out", ClassTimeout},
	{"timeout", ClassTimeout},
	{"context deadline exceeded", ClassTimeout},
	{"not ready within", ClassTimeout},

	{"persistentvolumeclaim", ClassStorage},
	{"no persistent volumes", ClassStorage},
	{"storageclass", ClassStorage},

	{"connection refused", ClassKubeAPI},
	{"the server could not find", ClassKubeAPI},
	{"unable to connect to the server", ClassKubeAPI},
	{"etcdserver", ClassKubeAPI},
	{"apiserver", ClassKubeAPI},
}

// ErrorClass buckets a failure message. An empty message is ClassNone, so an
// event carries "" where a handler recorded no error rather than claiming the
// failure was of an unrecognised kind.
func ErrorClass(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ClassNone
	}
	lower := strings.ToLower(msg)
	for _, m := range errorClassMarkers {
		if strings.Contains(lower, m.marker) {
			return m.class
		}
	}
	return ClassOther
}
