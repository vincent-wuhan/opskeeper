package pigprofile

import (
	"github.com/MichaelKinsy/PiG/coding/packagecontent"
)

// ExtensionPublicName is the name PiG will register a package's extension
// under, given the path the package declared for it.
//
// The node's profile names extensions in order to say which of their tools
// the model may be offered, and it matches those names against the source
// PiG stamps on each tool. The two therefore have to be the same string, and
// the string is PiG's to decide: it strips a file extension, and it maps
// index/main/extension entry points onto their containing directory. An
// OpsKeeper reimplementation of that rule would be correct on the day it
// was written and silently wrong the first time PiG adds a case to it —
// and "silently" is the whole problem, because a wrong name does not fail,
// it produces a profile that offers the tools of an extension nobody
// matched.
//
// This function lives here because this is the only module permitted to
// import PiG. core/floor reads the declared paths and cannot answer the
// question, so the answer is asked here and passed in.
func ExtensionPublicName(resourcePath string) (string, error) {
	return packagecontent.PublicName(packagecontent.Extensions, resourcePath, "")
}
