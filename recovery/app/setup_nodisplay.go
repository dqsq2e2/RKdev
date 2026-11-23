//go:build rkdev_nodisplay

package main

import "fmt"

func runSetup() error { return fmt.Errorf("此板包未启用本地界面") }
