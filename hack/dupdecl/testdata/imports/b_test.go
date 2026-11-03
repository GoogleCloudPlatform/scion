package p

import "strings"

var _ = strings.ToLower

func strings() {}

var rand = 1

const str = "x"

type yaml struct{}

// Not reported: a blank import declares no name; dot imports need type
// information (a documented limit, see main.go).
func embed() {}

func Println() {}

// A method is not in the package block.
func (yaml) strings() {}

func jose() {}

var gax, iam, iamcredentials int

// The guess for my-lib is not an identifier: no clash is reported.
func lib() {}

func my() {}
