package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func preprocessArgs(args []string) []string {
	result := make([]string, 0, len(args)+1)
	i := 0
	for i < len(args) {
		arg := args[i]
		if (arg == "--from" || arg == "--to") && i+1 < len(args) {
			result = append(result, arg, args[i+1])
			i += 2
			continue
		}
		if strings.HasPrefix(arg, "--from=") || strings.HasPrefix(arg, "--to=") {
			result = append(result, arg)
			i++
			continue
		}
		result = append(result, "--")
		result = append(result, args[i:]...)
		break
	}
	return result
}

func run(args []string, stdout, stderr *os.File) int {
	processed := preprocessArgs(args)

	fs := flag.NewFlagSet("tempconv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "source temperature scale (celsius, fahrenheit, kelvin)")
	to := fs.String("to", "", "target temperature scale (celsius, fahrenheit, kelvin)")

	if err := fs.Parse(processed); err != nil {
		return 1
	}

	if *from == "" || *to == "" {
		fmt.Fprintln(stderr, "usage: tempconv --from <scale> --to <scale> <value>")
		return 1
	}

	positional := fs.Args()
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: tempconv --from <scale> --to <scale> <value>")
		return 1
	}

	value, err := strconv.ParseFloat(positional[0], 64)
	if err != nil {
		fmt.Fprintf(stderr, "invalid temperature value: %s\n", positional[0])
		return 1
	}

	if math.IsNaN(value) || math.IsInf(value, 0) {
		fmt.Fprintf(stderr, "invalid temperature value: %s\n", positional[0])
		return 1
	}

	fromScale, err := parseScale(*from)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	toScale, err := parseScale(*to)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	result, err := convert(value, fromScale, toScale)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	fmt.Fprintf(stdout, "%.2f\n", result)
	return 0
}
