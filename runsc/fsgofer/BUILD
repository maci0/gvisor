load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "fsgofer",
    srcs = [
        "lisafs.go",
        "lisafs_compat_darwin.go",
        "lisafs_compat_linux.go",
    ],
    visibility = [
        "//cmd/sentrydarwin:__pkg__",
        "//runsc:__subpackages__",
    ],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/cleanup",
        "//pkg/fd",
        "//pkg/fsutil",
        "//pkg/lisafs",
        "//pkg/log",
        "//pkg/marshal/primitive",
        "//runsc/config",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "lisafs_test",
    size = "small",
    srcs = ["lisafs_test.go"],
    deps = [
        ":fsgofer",
        "//pkg/lisafs",
        "//pkg/lisafs/testsuite",
        "//pkg/log",
    ],
)
