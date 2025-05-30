// Copyright © 2022-2025 Obol Labs Inc. Licensed under the terms of a Business Source License 1.1

// Code generated by mockery v2.42.1. DO NOT EDIT.

package mocks

import (
	context "context"

	mock "github.com/stretchr/testify/mock"
)

// EthClientRunner is an autogenerated mock type for the EthClientRunner type
type EthClientRunner struct {
	mock.Mock
}

// Run provides a mock function with given fields: ctx
func (_m *EthClientRunner) Run(ctx context.Context) {
	_m.Called(ctx)
}

// VerifySmartContractBasedSignature provides a mock function with given fields: contractAddress, hash, sig
func (_m *EthClientRunner) VerifySmartContractBasedSignature(contractAddress string, hash [32]byte, sig []byte) (bool, error) {
	ret := _m.Called(contractAddress, hash, sig)

	if len(ret) == 0 {
		panic("no return value specified for VerifySmartContractBasedSignature")
	}

	var r0 bool
	var r1 error
	if rf, ok := ret.Get(0).(func(string, [32]byte, []byte) (bool, error)); ok {
		return rf(contractAddress, hash, sig)
	}
	if rf, ok := ret.Get(0).(func(string, [32]byte, []byte) bool); ok {
		r0 = rf(contractAddress, hash, sig)
	} else {
		r0 = ret.Get(0).(bool)
	}

	if rf, ok := ret.Get(1).(func(string, [32]byte, []byte) error); ok {
		r1 = rf(contractAddress, hash, sig)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// NewEthClientRunner creates a new instance of EthClientRunner. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewEthClientRunner(t interface {
	mock.TestingT
	Cleanup(func())
}) *EthClientRunner {
	mock := &EthClientRunner{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
