package fuzzoracle

/*
#cgo CFLAGS: -I${SRCDIR}/../vendor/libriscv/c
#cgo LDFLAGS: -L${SRCDIR}/../vendor/libriscv/build_capi -L${SRCDIR}/../vendor/libriscv/build_capi/libriscv -lriscv_capi -lriscv -lstdc++ -lm
#cgo darwin LDFLAGS: -framework Security
*/
import "C"
