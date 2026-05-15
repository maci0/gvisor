load("@io_bazel_rules_go//go:def.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "machexc",
    srcs = select({
        "@io_bazel_rules_go//go/platform:darwin_arm64": ["machexc_darwin.go"],
        "//conditions:default": ["machexc_other.go"],
    }),
    cgo = True,
    importpath = "gvisor.dev/gvisor/pkg/safecopy/machexc",
    visibility = ["//pkg/safecopy:__pkg__"],
)
