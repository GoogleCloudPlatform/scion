package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

func main() {
	from := flag.String("from", "", "source temperature scale (celsius, fahrenheit, kelvin)")
	to := flag.String("to", "", "target temperature scale (celsius, fahrenheit, kelvin)")
	flag.Parse()

	if *from == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "usage: tempconv --from <scale> --to <scale> <value>")
		os.Exit(1)
	}

	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: tempconv --from <scale> --to <scale> <value>")
		os.Exit(1)
	}

	value, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid temperature value: %s\n", args[0])
		os.Exit(1)
	}

	fromScale, err := parseScale(*from)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	toScale, err := parseScale(*to)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	result, err := convert(value, fromScale, toScale)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("%.2f\n", result)
}
