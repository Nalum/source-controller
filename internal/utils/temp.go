package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TmpDirForObj creates a new temporary directory in the directory dir
// in the format of 'Kind-Namespace-Name-*', and returns the
// pathname of the new directory.
func TmpDirForObj(dir string, obj client.Object) (string, error) {
	return os.MkdirTemp(dir, pattern(obj))
}

// TmpPathForObj creates a temporary file path in the format of
// '<dir>/Kind-Name-Namespace-<random bytes><suffix>'.
// If the given dir is empty, os.TempDir is used as a default.
func TmpPathForObj(dir, suffix string, obj client.Object) string {
	if dir == "" {
		dir = os.TempDir()
	}
	randBytes := make([]byte, 16)
	rand.Read(randBytes)
	return filepath.Join(dir, pattern(obj)+hex.EncodeToString(randBytes)+suffix)
}

func pattern(obj client.Object) (p string) {
	return fmt.Sprintf("%s-%s-%s-", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
}
