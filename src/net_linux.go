package main

import (
	"net"
	"syscall"
)

const (
	solSocket = syscall.SOL_SOCKET
	soBroadcast = syscall.SO_BROADCAST
)

// setBroadcast 开启 UDP 广播权限（WOL 魔术包必需）
func setBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), solSocket, soBroadcast, 1)
}

// connRefused 判断错误是否为"连接被拒绝"（主机在线、端口关闭）
func connRefused(err error) bool {
	if oe, ok := err.(*net.OpError); ok {
		if sys, ok := oe.Err.(syscall.Errno); ok {
			return sys == syscall.ECONNREFUSED
		}
	}
	return false
}
