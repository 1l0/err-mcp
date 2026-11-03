//go:build !js

package mcp

import (
	_ "embed"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// domainFileName is the proto file the descriptor was compiled from. It is the
// app's protocol definition and the single source of the RPC vocabulary.
const domainFileName = "domain.proto"

// domainDescriptorSet is the compiled FileDescriptorSet of the app's protocol.
//
// It is committed as a build input instead of being generated at build time, so
// this module stays a standalone Go command: no code generation, no dependency
// on generated Go packages, and nothing to run before "go install".
// Regenerate it with the app's protocol tooling when the schema changes;
// TestCatalogFingerprint pins the expected value.
//
//go:embed domain.fds
var domainDescriptorSet []byte

var (
	domainOnce sync.Once
	domainFile protoreflect.FileDescriptor
	domainErr  error
)

// domainDescriptor returns the embedded domain.proto descriptor.
func domainDescriptor() (protoreflect.FileDescriptor, error) {
	domainOnce.Do(func() {
		var set descriptorpb.FileDescriptorSet
		if err := proto.Unmarshal(domainDescriptorSet, &set); err != nil {
			domainErr = fmt.Errorf("decode embedded %s descriptor: %w", domainFileName, err)
			return
		}
		files, err := protodesc.NewFiles(&set)
		if err != nil {
			domainErr = fmt.Errorf("load embedded %s descriptor: %w", domainFileName, err)
			return
		}
		file, err := files.FindFileByPath(domainFileName)
		if err != nil {
			domainErr = fmt.Errorf("find %s in the embedded descriptor set: %w", domainFileName, err)
			return
		}
		domainFile = file
	})
	return domainFile, domainErr
}

// domainFileDescriptor is domainDescriptor for callers that cannot fail: the
// descriptor is embedded at build time and validated by the tests.
func domainFileDescriptor() protoreflect.FileDescriptor {
	file, err := domainDescriptor()
	if err != nil {
		panic(err)
	}
	return file
}

// lookupMethod resolves a godash RPC path such as "/pb.ItemService/GetItem" to
// its descriptor.
func lookupMethod(fullName string) (protoreflect.MethodDescriptor, bool) {
	file, err := domainDescriptor()
	if err != nil {
		return nil, false
	}
	for _, serviceName := range []string{"ItemService", "SyncService"} {
		service := file.Services().ByName(protoreflect.Name(serviceName))
		if service == nil {
			continue
		}
		for i := 0; i < service.Methods().Len(); i++ {
			method := service.Methods().Get(i)
			if "/"+string(service.FullName())+"/"+string(method.Name()) == fullName {
				return method, true
			}
		}
	}
	return nil, false
}

// lookupMessage resolves a fully qualified domain message name such as
// "pb.Item". Tests use it to build and inspect wire payloads without the app's
// generated Go types.
func lookupMessage(fullName protoreflect.FullName) (protoreflect.MessageDescriptor, bool) {
	file, err := domainDescriptor()
	if err != nil {
		return nil, false
	}
	messages := file.Messages()
	for i := 0; i < messages.Len(); i++ {
		if message := messages.Get(i); message.FullName() == fullName {
			return message, true
		}
	}
	return nil, false
}
