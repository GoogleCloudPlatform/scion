package main

import (
	"fmt"
	"strings"
)

type Scale int

const (
	Celsius Scale = iota
	Fahrenheit
	Kelvin
)

const absoluteZeroCelsius = -273.15

func parseScale(s string) (Scale, error) {
	switch strings.ToLower(s) {
	case "celsius", "c":
		return Celsius, nil
	case "fahrenheit", "f":
		return Fahrenheit, nil
	case "kelvin", "k":
		return Kelvin, nil
	default:
		return 0, fmt.Errorf("unknown temperature scale: %q", s)
	}
}

func toCelsius(value float64, from Scale) float64 {
	switch from {
	case Fahrenheit:
		return (value - 32) * 5 / 9
	case Kelvin:
		return value - 273.15
	default:
		return value
	}
}

func fromCelsius(c float64, to Scale) float64 {
	switch to {
	case Fahrenheit:
		return c*9/5 + 32
	case Kelvin:
		return c + 273.15
	default:
		return c
	}
}

func convert(value float64, from, to Scale) (float64, error) {
	c := toCelsius(value, from)
	if c < absoluteZeroCelsius {
		return 0, fmt.Errorf("temperature %.2f %s is below absolute zero", value, scaleName(from))
	}
	return fromCelsius(c, to), nil
}

func scaleName(s Scale) string {
	switch s {
	case Celsius:
		return "celsius"
	case Fahrenheit:
		return "fahrenheit"
	case Kelvin:
		return "kelvin"
	default:
		return "unknown"
	}
}
