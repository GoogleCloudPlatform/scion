// Package consumer imports the target package.
package consumer

import "example.com/fx/hub/sub"

// View wraps a policy.
type View struct{ P *sub.Policy }

// Name is the consumer's default.
const Name = "consumer"
