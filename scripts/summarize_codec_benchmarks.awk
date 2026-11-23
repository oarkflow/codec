BEGIN { OFS = " " }

/^Benchmark(Marshal|Unmarshal)\// {
	name = $1
	sub(/-[0-9]+$/, "", name)
	parts_n = split(name, parts, "/")
	if (parts_n != 3) next
	impl = parts[3]
	if (impl == "codec-nocopy") next
	if (impl != "codec" && impl != "protobuf" && impl != "cbor" && impl !~ /^msgpack-/ && impl != "json") next
	if ($0 !~ /ns\/op/) next
	time = 0
	allocs = "?"
	for (i = 2; i <= NF; i++) {
		if ($(i) == "ns/op") time = $(i - 1) + 0
		if ($(i) == "allocs/op") allocs = $(i - 1) + 0
	}
	if (time <= 0) next
	key = parts[1] SUBSEP parts[2] SUBSEP impl
	count[key]++
	values[key, count[key]] = time
	alloc_values[key, count[key]] = allocs
}

END {
	print "\nMeasured wire-format comparison summary (median ns/op; lower is faster):"
	for (oi = 1; oi <= 2; oi++) {
		op = oi == 1 ? "BenchmarkMarshal" : "BenchmarkUnmarshal"
		for (wi = 1; wi <= 3; wi++) {
			work = wi == 1 ? "doc" : (wi == 2 ? "batch1000" : "dynamic")
		codec_key = op SUBSEP work SUBSEP "codec"
		if (!count[codec_key]) continue
		codec_time = median(codec_key)
		codec_allocs = median_allocs(codec_key)
		best_impl = ""
		best_time = 0
		for (candidate in count) {
			split(candidate, keys, SUBSEP)
			if (keys[1] != op || keys[2] != work || keys[3] == "codec") continue
			candidate_time = median(candidate)
			if (best_impl == "" || candidate_time < best_time) {
				best_impl = keys[3]
				best_time = candidate_time
			}
		}
		if (best_impl == "") continue
		if (codec_time <= best_time) {
			lead = (best_time / codec_time - 1) * 100
			printf "  %s %s: Codec fastest at %.0f ns/op (%s allocs); %.0f%% ahead of %s (%.0f ns/op)\n", op, work, codec_time, codec_allocs, lead, best_impl, best_time
		} else {
			gap = (codec_time / best_time - 1) * 100
			printf "  %s %s: %s leads at %.0f ns/op; Codec %.0f ns/op (%.0f%% slower)\n", op, work, best_impl, best_time, codec_time, gap
		}
		}
	}
}

function median(key,    n, i, j, v, tmp) {
	n = count[key]
	for (i = 1; i <= n; i++) tmp[i] = values[key, i]
	for (i = 2; i <= n; i++) {
		v = tmp[i]
		j = i - 1
		while (j >= 1 && tmp[j] > v) { tmp[j + 1] = tmp[j]; j-- }
		tmp[j + 1] = v
	}
	return n % 2 ? tmp[(n + 1) / 2] : (tmp[n / 2] + tmp[n / 2 + 1]) / 2
}

function median_allocs(key,    n, i, j, v, tmp) {
	n = count[key]
	for (i = 1; i <= n; i++) tmp[i] = alloc_values[key, i]
	for (i = 2; i <= n; i++) {
		v = tmp[i]
		j = i - 1
		while (j >= 1 && tmp[j] > v) { tmp[j + 1] = tmp[j]; j-- }
		tmp[j + 1] = v
	}
	return n % 2 ? tmp[(n + 1) / 2] : (tmp[n / 2] + tmp[n / 2 + 1]) / 2
}
