// Command cmp runs benchmarks and compares them with bench/baseline.json;
// a regression of more than 15% fails (CLAUDE.md §11). With -update it
// rewrites the baseline instead.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
)

var lineRE = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op(?:\s+([\d.]+) MB/s)?`)

func main() {
	baseline := flag.String("baseline", "bench/baseline.json", "baseline file")
	update := flag.Bool("update", false, "write the baseline instead of comparing")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cmp [-baseline f] [-update] -- go test -bench ... pkgs")
		os.Exit(2)
	}
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...) // #nosec G204 -- Makefile-controlled
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fail(err)
	}
	if err := cmd.Start(); err != nil {
		fail(err)
	}
	results := map[string]float64{}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		fmt.Println(line)
		if m := lineRE.FindStringSubmatch(line); m != nil {
			ns, _ := strconv.ParseFloat(m[2], 64)
			results[m[1]] = ns
		}
	}
	if err := cmd.Wait(); err != nil {
		fail(err)
	}
	if len(results) == 0 {
		fail(errors.New("no benchmark results in the output: nothing was compared"))
	}
	if *update {
		b, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(*baseline, append(b, '\n'), 0o644); err != nil { // #nosec G306 -- not secret
			fail(err)
		}
		fmt.Println("baseline written:", *baseline)
		return
	}
	var base map[string]float64
	b, err := os.ReadFile(*baseline)
	if err != nil {
		fail(fmt.Errorf("no baseline (%w); run with -update first", err))
	}
	if err := json.Unmarshal(b, &base); err != nil {
		fail(err)
	}
	names := make([]string, 0, len(results))
	for n := range results {
		names = append(names, n)
	}
	sort.Strings(names)
	bad := 0
	for _, n := range names {
		ref, ok := base[n]
		if !ok {
			fmt.Printf("%-40s %10.0f ns/op (new, no baseline)\n", n, results[n])
			continue
		}
		delta := (results[n] - ref) / ref * 100
		flag := ""
		if delta > 15 {
			flag = "  REGRESSION"
			bad++
		}
		fmt.Printf("%-40s %10.0f ns/op  baseline %10.0f  %+6.1f%%%s\n", n, results[n], ref, delta, flag)
	}
	// A benchmark that was renamed or removed must not drop out of the check unnoticed.
	for n := range base {
		if _, ran := results[n]; !ran {
			fmt.Printf("%-40s did not run (in the baseline; update it with -update if that is intended)\n", n)
			bad++
		}
	}
	if bad > 0 {
		fail(fmt.Errorf("%d benchmark(s) regressed more than 15%% or did not run", bad))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "bench/cmp:", err)
	os.Exit(1)
}
