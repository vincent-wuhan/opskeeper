// Module base is the control plane's shared infrastructure, one tier above
// the contracts and one below the control plane proper.
//
// It exists because `pkg` was the reason manager could not be cut into
// independently evolvable pieces. Every domain in the control plane rests on
// it — 45 of them, by production import — so while it sat inside the manager
// module, no domain could leave without dragging the whole floor along, and
// the release floor (the 28 domains nothing imports) was priced as
// extractable when it was not: it rested on a package that was still inside
// the thing being split.
//
// What is here is the set of packages the control plane runs on and that know
// nothing about any bounded context: errors, tenant context, credential
// injection, the audit write port, database and cache plumbing, fanout, the
// leader lease, and the adapters for the systems the platform reads. Nothing
// here names a domain.
//
// What is deliberately not here:
//
//   - Business vocabulary. `floor` refuses it and so does this module; a
//     Prometheus client belongs here, an alert does not.
//   - Anything the node plane needs. `floor` is what both planes share, and
//     putting go-redis, JWT, fastembed and a PDF reader in there would make
//     every plugin on every node resolve infrastructure it never calls. That
//     is the reason this is a separate module from floor rather than a
//     subdirectory of it.
module github.com/vincent-wuhan/opskeeper/core/base

go 1.26.0

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/anush008/fastembed-go v1.0.0
	github.com/casbin/casbin/v2 v2.103.0
	github.com/casbin/gorm-adapter/v3 v3.32.0
	github.com/glebarez/go-sqlite v1.22.0
	github.com/glebarez/sqlite v1.11.0
	github.com/go-chi/chi/v5 v5.1.0
	github.com/golang-jwt/jwt/v5 v5.3.0
	github.com/golang/snappy v0.0.4
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgx/v5 v5.8.0
	github.com/larksuite/oapi-sdk-go/v3 v3.5.4
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728
	github.com/prometheus/client_golang v1.20.5
	github.com/prometheus/client_model v0.6.1
	github.com/redis/go-redis/v9 v9.21.0
	github.com/robfig/cron/v3 v3.0.1
	github.com/segmentio/kafka-go v0.4.51
	github.com/singchia/frontier v1.2.3-rc.2
	github.com/singchia/geminio v1.2.3-rc.1
	github.com/stretchr/testify v1.11.1
	github.com/vincent-wuhan/opskeeper/core v0.0.0
	github.com/vincent-wuhan/opskeeper/core/floor v0.0.0
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/trace v1.43.0
	github.com/philippgille/chromem-go v0.7.0
	golang.org/x/crypto v0.55.0
	golang.org/x/sync v0.22.0
	golang.org/x/time v0.15.0
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
	gorm.io/driver/mysql v1.6.0
	gorm.io/driver/postgres v1.6.0
	gorm.io/gorm v1.31.1
	gorm.io/plugin/soft_delete v1.2.1
)

require (
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/Masterminds/semver/v3 v3.5.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/aws/aws-sdk-go-v2 v1.41.7 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.10 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.17 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.16 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.50.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.42.1 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/casbin/govaluate v1.10.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/x/ansi v0.11.7 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/deckarep/golang-set/v2 v2.6.0 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-kratos/aegis v0.2.0 // indirect
	github.com/go-kratos/kratos/v2 v2.7.2 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-playground/form/v4 v4.2.0 // indirect
	github.com/go-sql-driver/mysql v1.9.3 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/jumboframes/armorigo v0.4.1 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.23 // indirect
	github.com/microsoft/go-mssqldb v1.9.5 // indirect
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/schollz/progressbar/v2 v2.15.0 // indirect
	github.com/schollz/progressbar/v3 v3.14.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/singchia/go-timer/v2 v2.2.1 // indirect
	github.com/singchia/yafsm v1.0.1 // indirect
	github.com/sugarme/regexpset v0.0.0-20200920021344-4d4ec8eaf93c // indirect
	github.com/sugarme/tokenizer v0.2.3-0.20230829214935-448e79b1ed65 // indirect
	github.com/yalue/onnxruntime_go v1.7.0 // indirect
	github.com/yuin/goldmark v1.8.5 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/image v0.45.0 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	gorm.io/driver/sqlserver v1.6.3 // indirect
	gorm.io/plugin/dbresolver v1.6.2 // indirect
	k8s.io/klog/v2 v2.120.1 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.53.0 // indirect
)

// Sibling modules resolve by path during development; the workspace covers
// this in a normal build, the replaces keep a bare module directory
// buildable in CI jobs that disable workspaces.
replace (
	github.com/vincent-wuhan/opskeeper/core => ../
	github.com/vincent-wuhan/opskeeper/core/floor => ../floor
	github.com/vincent-wuhan/opskeeper/sdk => ../../sdk
)
