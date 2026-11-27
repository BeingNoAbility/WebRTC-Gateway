//go:build unix

package proxy

import (
	"os"
	"syscall"
)

// openRootNoFollowNoBlock opens a resolved symlink-free path inside os.Root in a single syscall
// with O_RDONLY|O_NONBLOCK|O_NOFOLLOW (eliminates both FIFO DoS and symlink swap TOCTOU).
func openRootNoFollowNoBlock(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
