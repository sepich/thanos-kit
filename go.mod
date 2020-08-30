module github.com/sepich/thanos-kit

go 1.15

require (
	github.com/go-kit/kit v0.10.0
	github.com/oklog/ulid v1.3.1
	github.com/olekukonko/tablewriter v0.0.2
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.6.0
	github.com/prometheus/common v0.10.0
	github.com/prometheus/prometheus v1.8.2-0.20200629082805-315564210816
	github.com/thanos-io/thanos v0.14.0
	golang.org/x/text v0.3.2
	gopkg.in/alecthomas/kingpin.v2 v2.2.6
)

// https://github.com/golang/go/issues/33558
replace k8s.io/client-go => k8s.io/client-go v0.0.0-20190620085101-78d2af792bab
