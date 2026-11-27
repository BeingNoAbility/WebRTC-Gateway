//go:build !unix

package proxy

import "os"

func openRootNoFollowNoBlock(root *os.Root, rel string) (*os.File, error) {
	return root.Open(rel)
}
