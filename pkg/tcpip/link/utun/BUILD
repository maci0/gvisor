load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "utun",
    srcs = [
        "endpoint.go",
        "proxy.go",
    ],
    stateify = False,
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/buffer",
        "//pkg/log",
        "//pkg/tcpip",
        "//pkg/tcpip/header",
        "//pkg/tcpip/link/channel",
        "//pkg/tcpip/stack",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
