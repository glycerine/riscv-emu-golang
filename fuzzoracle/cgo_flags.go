package fuzzoracle

/*
#cgo CFLAGS: -I${SRCDIR}/../xendor/libriscv/c
#cgo LDFLAGS: -L${SRCDIR}/../xendor/libriscv/build_capi -L${SRCDIR}/../xendor/libriscv/build_capi/libriscv -lriscv_capi -lriscv -lstdc++ -lm
#cgo darwin LDFLAGS: -framework Security
*/
import "C"
