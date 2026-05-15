load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "unet",
    srcs = [
        "unet.go",
        "unet_darwin.go",
        "unet_linux.go",
        "unet_unsafe.go",
        "unet_unsafe_darwin.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/atomicbitops",
        "//pkg/eventfd",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "unet_test",
    size = "small",
    srcs = [
        "unet_test.go",
    ],
    library = ":unet",
    deps = [
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
