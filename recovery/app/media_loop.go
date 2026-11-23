package main

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Linux uapi/linux/loop.h. LOOP_CONFIGURE binds the opened file and the
// extent atomically; AUTOCLEAR frees the loop after its last user closes it.
type loopConfig struct {
	fd, blockSize                             uint32
	device, inode, rdevice, offset, sizeLimit uint64
	number, encryption, keySize, flags        uint32
	fileName, cryptName                       [64]byte
	encryptionKey                             [32]byte
	init                                      [2]uint64
	reserved                                  [8]uint64
}

func checkMediaRecoveryPayload(ctx context.Context, image *os.File, l runtimeLayout) error {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("无法打开只读镜像校验设备: %w", err)
	}
	defer ctl.Close()
	for range 6 {
		n, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ctl.Fd(), 0x4c82, 0) // LOOP_CTL_GET_FREE
		if errno != 0 {
			return errno
		}
		dev, err := os.OpenFile(fmt.Sprintf("/dev/loop%d", n), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		cfg := loopConfig{fd: uint32(image.Fd()), offset: uint64(l.Recovery.Offset), sizeLimit: uint64(l.Recovery.Length), flags: 1 | 4}
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, dev.Fd(), 0x4c0a, uintptr(unsafe.Pointer(&cfg))) // LOOP_CONFIGURE
		if errno != 0 {
			dev.Close()
			if errno == syscall.EBUSY {
				continue
			}
			return fmt.Errorf("无法映射镜像恢复区: %w", errno)
		}
		defer dev.Close()
		return checkRecoveryPayload(ctx, "/proc/self/fd/3", dev, l)
	}
	return fmt.Errorf("只读镜像校验设备正在使用，请重试")
}
