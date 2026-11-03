package p

import "strings"

var _ = strings.ToLower

func strings() {}

var rand = 1

const str = "x"

type yaml struct{}

// Not reported: blank and dot imports declare no file-block name.
func embed() {}

func Println() {}

// A method is not in the package block.
func (yaml) strings() {}
