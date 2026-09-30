// Command cmp runs benchmarks, takes the median of each one's samples (run
// them with -count=5), and fails when a median breaks an absolute budget of
// CLAUDE.md §11 or is more than 15% slower than bench/baseline.json. With
// -update it rewrites the baseline instead of comparing.
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
	"slices"
	"strconv"
)

var lineRE = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op(?:\s+([\d.]+) MB/s)?`)

// budgets are the §11 limits that the benchmarks measure. A budget whose
// benchmarks did not run is skipped; the baseline check reports them.
var budgets = []struct {
	what  string
	needs []string
	ok    func(ns, mbs map[string]float64) bool
}{
	{"handshake, both sides: < 10 ms (5 ms per side)", []string{"BenchmarkHandshake"},
		func(ns, _ map[string]float64) bool { return ns["BenchmarkHandshake"] < 10e6 }},
	{"latency a TEXT frame adds over raw TCP: < 1 ms", []string{"BenchmarkTextOverSession", "BenchmarkTextOverRawTCP"},
		func(ns, _ map[string]float64) bool {
			return ns["BenchmarkTextOverSession"]-ns["BenchmarkTextOverRawTCP"] < 1e6
		}},
	{"AEAD and chain throughput, full chunks: ≥ 300 MB/s", []string{"BenchmarkSealOpen"},
		func(_, mbs map[string]float64) bool { return mbs["BenchmarkSealOpen"] >= 300 }},
	{"transfer throughput: ≥ 90 MB/s", []string{"BenchmarkTransfer"},
		func(_, mbs map[string]float64) bool { return mbs["BenchmarkTransfer"] >= 90 }},
	{"rekey, both sides and the round trips: < 1.5 ms", []string{"BenchmarkRekey"},
		func(ns, _ map[string]float64) bool { return ns["BenchmarkRekey"] < 1.5e6 }},
}

func main() {
	baseline := flag.String("baseline", "bench/baseline.json", "baseline file")
	update := flag.Bool("update", false, "write the baseline instead of comparing")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cmp [-baseline f] [-update] -- go test -bench ... -count=5 pkgs")
		os.Exit(2)
	}
	ns, mbs, err := run(args)
	if err != nil {
		fail(err)
	}
	if *update {
		b, _ := json.MarshalIndent(ns, "", "  ")
		if err := os.WriteFile(*baseline, append(b, '\n'), 0o644); err != nil { // #nosec G306 -- not secret
			fail(err)
		}
		fmt.Println("baseline written:", *baseline)
		return
	}
	bad := 0
	for _, b := range budgets {
		ran := true
		for _, n := range b.needs {
			_, ok := ns[n]
			ran = ran && ok
		}
		switch {
		case !ran:
		case b.ok(ns, mbs):
			fmt.Println("budget met:    ", b.what)
		default:
			fmt.Println("BUDGET BROKEN: ", b.what)
			bad++
		}
	}
	bad += compare(*baseline, ns)
	if bad > 0 {
		fail(fmt.Errorf("%d check(s) failed: a budget broken, a regression over 15%%, or a benchmark that did not run", bad))
	}
}

// run runs the benchmark command, echoes its output, and returns the median
// ns/op and MB/s of every benchmark.
func run(args []string) (ns, mbs map[string]float64, err error) {
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...) // #nosec G204 -- Makefile-controlled
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	nsAll, mbsAll := map[string][]float64{}, map[string][]float64{}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		fmt.Println(line)
		if m := lineRE.FindStringSubmatch(line); m != nil {
			v, _ := strconv.ParseFloat(m[2], 64)
			nsAll[m[1]] = append(nsAll[m[1]], v)
			if m[3] != "" {
				v, _ := strconv.ParseFloat(m[3], 64)
				mbsAll[m[1]] = append(mbsAll[m[1]], v)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, nil, err
	}
	if len(nsAll) == 0 {
		return nil, nil, errors.New("no benchmark results in the output: nothing was compared")
	}
	return medians(nsAll), medians(mbsAll), nil
}

func medians(samples map[string][]float64) map[string]float64 {
	out := make(map[string]float64, len(samples))
	for n, s := range samples {
		slices.Sort(s)
		out[n] = s[len(s)/2]
	}
	return out
}

// compare prints each median against the baseline and returns how many
// regressed by more than 15% or did not run.
func compare(path string, ns map[string]float64) int {
	var base map[string]float64
	b, err := os.ReadFile(path) // #nosec G304 -- Makefile-controlled
	if err != nil {
		fail(fmt.Errorf("no baseline (%w); run with -update first", err))
	}
	if err := json.Unmarshal(b, &base); err != nil {
		fail(err)
	}
	bad := 0
	names := make([]string, 0, len(ns))
	for n := range ns {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		ref, ok := base[n]
		if !ok {
			fmt.Printf("%-40s %10.0f ns/op (new, no baseline)\n", n, ns[n])
			continue
		}
		delta := (ns[n] - ref) / ref * 100
		mark := ""
		if delta > 15 {
			mark = "  REGRESSION"
			bad++
		}
		fmt.Printf("%-40s %10.0f ns/op  baseline %10.0f  %+6.1f%%%s\n", n, ns[n], ref, delta, mark)
	}
	// A benchmark that was renamed or removed must not drop out of the check unnoticed.
	for n := range base {
		if _, ran := ns[n]; !ran {
			fmt.Printf("%-40s did not run (in the baseline; update it with -update if that is intended)\n", n)
			bad++
		}
	}
	return bad
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "bench/cmp:", err)
	os.Exit(1)
}
