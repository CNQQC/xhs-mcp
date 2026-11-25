package main

import (
	"testing"

	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func fullBenchmarkTable() *refTable {
	refs := newRefTable()
	for range refMaxEntries {
		refs.put(refTarget{FeedID: "seed"})
	}
	return refs
}

func BenchmarkFullTablePut(b *testing.B) {
	refs := fullBenchmarkTable()
	b.ReportAllocs()
	for b.Loop() {
		refs.put(refTarget{FeedID: "next"})
	}
}

func BenchmarkFullTableList20(b *testing.B) {
	refs := fullBenchmarkTable()
	feeds := make([]xiaohongshu.Feed, 20)
	for index := range feeds {
		feeds[index] = sampleFeed()
	}
	b.ReportAllocs()
	for b.Loop() {
		toNoteListView(refs, feeds, "search")
	}
}
