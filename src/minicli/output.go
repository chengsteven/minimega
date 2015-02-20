// Copyright (2015) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package minicli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"ranges"
	"sort"
	"strings"
	"text/tabwriter"
)

type table [][]string

func (t table) Len() int {
	return len(t)
}

func (t table) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
}

func (t table) Less(i, j int) bool {
	return t[i][0] < t[j][0]
}

// Return a string representation using the current output mode
func (r Responses) String() string {
	if len(r) == 0 {
		return ""
	}

	if mode == jsonMode {
		bytes, err := json.Marshal(r)
		if err != nil {
			// TODO: Should this be JSON-formatted too?
			return err.Error()
		}

		return string(bytes)
	}

	header, err := r.getHeader()
	if err != nil {
		return err.Error()
	}

	// TODO: What is Header for simple responses?

	tabular, err := r.validTabular(header)
	if err != nil {
		return err.Error()
	}

	var count int
	for _, x := range r {
		count += len(x.Tabular)
	}

	var buf bytes.Buffer

	if tabular && count > 0 {
		r.tabularString(&buf, header)
	} else if compress && len(r) > 1 {
		r.compressString(&buf)
	} else {
		for _, v := range r {
			if v.Error == "" && v.Response != "" {
				if annotate {
					buf.WriteString(v.Host)
					buf.WriteString(": ")
				}
				buf.WriteString(v.Response)
				buf.WriteString("\n")
			}
		}
	}

	// Append errors from hosts
	for i, v := range r {
		if v.Error != "" {
			fmt.Fprintf(&buf, "Error (%s): %s", v.Host, v.Error)

			// add a newline unless this is our last iteration
			if i != len(r)-1 {
				fmt.Fprintf(&buf, "\n")
			}
		}
	}

	resp := buf.String()
	return strings.TrimSpace(resp)
}

func (r Responses) tabularString(buf io.Writer, header []string) {
	// Add extra column to the data so that
	if annotate {
		header = append([]string{"host"}, header...)

		for i := range r {
			for j, row := range r[i].Tabular {
				r[i].Tabular[j] = append([]string{r[i].Host}, row...)
			}
		}
	}

	// Collect all the tabular data
	data := [][]string{}
	for i := range r {
		data = append(data, r[i].Tabular...)
	}

	sort.Sort(table(data))

	if mode == csvMode {
		printCSV(buf, header, data)
	} else {
		printTabular(buf, header, data)
	}
}

func (r Responses) compressString(buf io.Writer) {
	buckets := map[uint64]Responses{}

	// Find responses that have the same output by hashing them into buckets
	for _, v := range r {
		if v.Error == "" && v.Response != "" {
			h := fnv.New64a()
			h.Write([]byte(v.Response))
			k := h.Sum64()

			buckets[k] = append(buckets[k], v)
		}
	}

	if len(buckets) == 0 {
		return
	}

	// Compress hostnames into ranges
	ranges := map[string]uint64{}
	for hash, resps := range buckets {
		if len(resps) == 1 {
			ranges[resps[0].Host] = hash
			continue
		}

		hosts := []string{}
		for _, r := range resps {
			hosts = append(hosts, r.Host)
		}

		ranges[compressHosts(hosts)] = hash
	}

	// Sort the keys of ranges
	hosts := []string{}
	for k := range ranges {
		hosts = append(hosts, k)
	}
	sort.Strings(hosts)

	for _, h := range hosts {
		resp := buckets[ranges[h]][0]

		if annotate {
			buf.Write([]byte(h))
			buf.Write([]byte(": "))
		}

		buf.Write([]byte(resp.Response))
		buf.Write([]byte("\n"))
	}
}

func compressHosts(hosts []string) string {
	var res []string

	// Add all the hosts to a trie
	trie := newTrie()
	for _, v := range hosts {
		trie.Add(v)
	}
	prefixes := trie.AlphaPrefixes()

	// Find the longest prefix match for each host
	groups := map[string][]string{}
	for _, h := range hosts {
		longest := ""
		for _, p := range prefixes {
			if strings.HasPrefix(h, p) && len(p) > len(longest) {
				longest = p
			}
		}

		groups[longest] = append(groups[longest], h)
	}

	// Compress each group of hosts that share the same prefix
	for p, group := range groups {
		r, _ := ranges.NewRange(p, 0, int(math.MaxInt32))

		s, err := r.UnsplitRange(group)
		if err != nil {
			// Fallback, append all the hosts
			res = append(res, group...)
			continue
		}

		res = append(res, s)
	}

	sort.Strings(res)

	return strings.Join(res, ",")
}

func printTabular(buf io.Writer, header []string, data [][]string) {
	w := new(tabwriter.Writer)
	w.Init(buf, 5, 0, 1, ' ', 0)
	defer w.Flush()

	if headers {
		for i, h := range header {
			if i != 0 {
				fmt.Fprintf(w, "\t| ")
			}
			fmt.Fprintf(w, h)
		}
		fmt.Fprintf(w, "\n")
	}

	for _, row := range data {
		for i, v := range row {
			if i != 0 {
				fmt.Fprintf(w, "\t| ")
			}
			fmt.Fprintf(w, v)
		}
		fmt.Fprintf(w, "\n")
	}
}

func printCSV(buf io.Writer, header []string, data [][]string) {
	w := csv.NewWriter(buf)
	defer w.Flush()

	if headers {
		w.Write(header)
	}

	w.WriteAll(data)
}