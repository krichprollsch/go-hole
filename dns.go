package main

import (
	"fmt"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"log"
	"strings"
	"time"
)

var (
	queriesHistogram = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "dns",
			Name:      "queries_duration_seconds",
			Help:      "Duration of replies to DNS queries.",
			Buckets: []float64{
				1e-5, 2.5e-5, 5e-5, 7.5e-5,
				1e-4, 2.5e-4, 5e-4, 7.5e-4,
				1e-3, 2.5e-3, 5e-3, 7.5e-3,
				1e-2, 2.5e-2, 5e-2, 7.5e-2,
				1e-1,
			},
		},
		[]string{"status", "query"},
	)
)

// runDNSServer starts a custom DNS server that blocks the domains contained
// in a blacklist and answers the other queries using an upstream DNS server.
func runDNSServer() {

	// load the blocked domains
	blacklist := LoadBlacklistOrFail(blacklistPath)
	fmt.Printf("Loading list of %d blocked domains...\n", blacklist.Size())

	// make the custom handler function to reply to DNS queries
	upstream := getEnvOrDefault("UPSTREAM_DNS", "1.1.1.1:53")
	logging := getEnvOrDefault("DEBUG", "") == "true"
	handler := makeDNSHandler(blacklist, upstream, logging)

	// start the server
	port := getEnvOrDefault("DNS_PORT", "53")
	fmt.Printf("Starting DNS server on UDP port %s (logging = %t)...\n", port, logging)
	server := &dns.Server{Addr: ":" + port, Net: "udp"}
	dns.HandleFunc(".", handler)
	err := server.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}

// makeDNSHandler creates an handler for the DNS server that caches
// results from the upstream DNS and blocks domains in the blacklist.
func makeDNSHandler(blacklist *Blacklist, upstream string, logging bool) func(dns.ResponseWriter, *dns.Msg) {

	// create the logger function
	logger := func(res *dns.Msg, duration time.Duration, how string) {}
	if logging {
		logger = func(msg *dns.Msg, rtt time.Duration, how string) {
			log.Printf("Using %s, response time %s:\n%s\n", how, rtt.String(), msg.String())
		}
	}

	// cache for the DNS replies from the DNS server
	cache := NewCache()

	// we use a single client to resolve queries against the upstream DNS
	client := new(dns.Client)

	// create the real handler
	return func(w dns.ResponseWriter, req *dns.Msg) {
		start := time.Now()

		// the standard allows multiple DNS questions in a single query... but nobody uses it, so we disallow it
		// https://stackoverflow.com/questions/4082081/requesting-a-and-aaaa-records-in-single-dns-query/4083071
		if len(req.Question) != 1 {

			// reply with a format error
			res := new(dns.Msg)
			res.SetRcode(req, dns.RcodeFormatError)
			w.WriteMsg(res)

			// collect metrics
			duration := time.Since(start).Seconds()
			queriesHistogram.WithLabelValues("malformed_query", "-").Observe(duration)

			return
		}

		// extract the DNS question
		query := req.Question[0]
		domain := strings.TrimRight(query.Name, ".")
		queryType := dns.TypeToString[query.Qtype]

		// check the cache first: if a domain is in the cache, it cannot be blocked
		// this optimized response times for allowed domains over the blocked domains
		cached, found := cache.Get(&query)
		if found {

			// cache found, use the cached answer
			res := cached.SetReply(req)
			res.Answer = cached.Answer
			w.WriteMsg(res)

			// log the query
			duration := time.Since(start)
			logger(res, duration, "cache")

			// collect metrics
			durationSeconds := duration.Seconds()
			queriesHistogram.WithLabelValues("cache", queryType).Observe(durationSeconds)

			return
		}

		// then, check if the domain is blocked
		blocked := blacklist.Contains(domain)
		if blocked {

			// reply with "domain not found"
			res := new(dns.Msg)
			res.SetRcode(req, dns.RcodeNameError)
			w.WriteMsg(res)

			// log the query
			duration := time.Since(start)
			logger(res, duration, "block")

			// collect metrics
			durationSeconds := duration.Seconds()
			queriesHistogram.WithLabelValues("block", queryType).Observe(durationSeconds)

			return
		}

		// finally, query an upstream DNS
		res, rtt, err := client.Exchange(req, upstream)
		if err == nil {

			// reply to the query
			w.WriteMsg(res)

			// cache the result
			expiration := time.Duration(res.Answer[0].Header().Ttl) * time.Second
			cache.Set(&query, res, expiration)

			// log the query
			logger(res, rtt, "upstream")

			// collect metrics
			durationSeconds := time.Since(start).Seconds()
			queriesHistogram.WithLabelValues("upstream", queryType).Observe(durationSeconds)

		} else {

			// log the error
			if logging {
				log.Print(err)
			} else {
				log.Print("Error with upstream")
			}

			// collect metrics
			durationSeconds := time.Since(start).Seconds()
			queriesHistogram.WithLabelValues("upstream_error", queryType).Observe(durationSeconds)
		}
	}
}