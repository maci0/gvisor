load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "fdnotifier",
    srcs = [
        "fdnotifier.go",
        "fdnotifier_darwin.go",
        "poll_unsafe.go",
        "poll_unsafe_darwin.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/sync",
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
