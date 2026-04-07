//go:build !windows

package main

import "fmt"

func listDrives() {
	fmt.Println("Drive listing (-a) is Windows-only.")
	fmt.Println("On Linux, use: lsblk")
}
