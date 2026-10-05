//go:build !linux && !darwin && !windows

package service

import "fmt"

func Install(string) error                    { return fmt.Errorf("this operating system is not supported") }
func Uninstall() error                        { return fmt.Errorf("this operating system is not supported") }
func Active() (bool, error)                   { return false, fmt.Errorf("this operating system is not supported") }
func Stop() error                             { return fmt.Errorf("this operating system is not supported") }
func Start() error                            { return fmt.Errorf("this operating system is not supported") }
func RemoveInstalledBinary(path string) error { return removeFile(path) }
func AcquireRunLock(string) (func() error, error) {
	return nil, fmt.Errorf("this operating system is not supported")
}
