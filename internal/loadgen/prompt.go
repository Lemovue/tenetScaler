package loadgen

import (
	"math/rand/v2"
	"strings"
)

// vocabulary holds common English words. Each is a single token for typical
// BPE tokenizers (Qwen2.5, Llama) when preceded by a space, so a prompt of n
// words is close to n tokens on a real engine and exactly n tokens for the
// simulated engine and the gateway (engine.CountTokens counts words).
var vocabulary = strings.Fields(`
	time year people way day man thing woman life child world school state
	family student group country problem hand part place case week company
	system program question work number night point home water room mother
	area money story fact month lot study book eye job word business issue
	side kind head house service friend father power hour game line end member
	law car city name team minute idea body information back parent face level
	office door health person art war history party result change morning
	reason research girl moment air teacher force education foot boy age
	policy music market sense nation plan college interest death experience
	effect class control care field role effort rate heart show leader light
	voice wife police mind price report decision son view town road arm value
	building action model season society tax director position player record
	paper space ground form event matter center couple site project activity
	star table need court oil situation cost industry figure street image phone
	data picture practice piece land product doctor wall patient worker news
	test movie north love support step baby computer type attention film tree
	source hair window truth song river garden summer winter spring autumn
	ocean island forest mountain valley desert bridge castle village harbor
	kitchen bread apple orange lemon sugar salt coffee milk cheese butter
	honey rice soup bottle glass plate chair bed lamp clock mirror carpet
	pencil letter ticket camera engine wheel train plane ship boat horse
	bird fish dog cat sheep cow mouse lion tiger bear wolf snake bee
	flower grass leaf stone sand snow rain wind cloud storm sun moon sky fire
	smoke gold silver iron wood cotton paint color blue green red yellow brown
	white black small large quick slow happy quiet bright dark warm cold soft
	hard clean early late simple strong gentle brave calm proud wise young old
	fresh sweet bitter rich poor deep wide narrow thin thick heavy empty full
	walk run jump swim read write sing dance cook build open close carry pull
	push drive fly sleep watch listen speak learn teach travel visit draw
`)

// splitmix64 scrambles x (Steele, Lea and Flood 2014) so that consecutive
// request indices seed unrelated random streams.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// Prompt returns the prompt of request index of a run with the given seed:
// max(words, 1) space-separated words from a vocabulary of a few hundred
// common English words. It is deterministic in (seed, index, words).
//
// Prompts are unique per request because llama.cpp reuses the KV cache of a
// prompt prefix it has already processed: replaying identical (or
// prefix-sharing) prompts would skip most of the prefill and flatter the
// latency numbers. The first two words encode index (offset by the seed),
// so among the first V*V requests of a run (V is the vocabulary size and V*V
// exceeds 100 thousand) no two prompts of two or more words begin with the
// same two words, so two prompts share at most a one-word prefix; the
// remaining words are drawn pseudo-randomly from a stream keyed by seed and
// index.
func Prompt(seed int64, index uint64, words int) string {
	words = max(words, 1)
	v := uint64(len(vocabulary))
	mix := splitmix64(uint64(seed))
	rng := rand.New(rand.NewPCG(mix, splitmix64(index^mix)))

	var b strings.Builder
	b.Grow(words * 8)
	for i := range words {
		var w uint64
		switch i {
		case 0:
			w = (index + mix) % v
		case 1:
			w = (index/v + mix>>32) % v
		default:
			w = rng.Uint64N(v)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(vocabulary[w])
	}
	return b.String()
}
