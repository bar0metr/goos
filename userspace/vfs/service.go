// Package vfsservice implements the bounded userspace VFS request dispatcher.
package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

type Service struct {
	fs        ramfs.FileSystem
	namespace vfs.Namespace
	readData  [vfs.MaxReplyData]byte
}

func (service *Service) Init() bool {
	root := service.fs.Init()
	system, status := service.fs.Mkdir(root, []byte("system"))
	if status != ramfs.StatusOK {
		return false
	}
	motd, status := service.fs.Create(system, []byte("motd"))
	if status != ramfs.StatusOK {
		return false
	}
	if _, status = service.fs.Write(motd, 0, []byte("Welcome to GoOS\n")); status != ramfs.StatusOK {
		return false
	}
	if _, status = service.fs.Mkdir(root, []byte("tmp")); status != ramfs.StatusOK {
		return false
	}
	return service.namespace.Mount([]byte("/"), &service.fs, root) == vfs.StatusOK
}

func (service *Service) Dispatch(request vfs.Request) vfs.Reply {
	reply := vfs.Reply{Operation: request.Operation}
	switch request.Operation {
	case vfs.OperationOpen:
		reply.Handle, reply.Status = service.namespace.Open(request.Path, request.Rights)
	case vfs.OperationClose:
		reply.Status = service.namespace.Close(request.Handle)
	case vfs.OperationRead:
		size := int(request.Size)
		if size > len(service.readData) {
			size = len(service.readData)
		}
		count, status := service.namespace.Read(request.Handle, service.readData[:size])
		reply.Status, reply.Data, reply.Size = status, service.readData[:count], uint64(count)
	case vfs.OperationWrite:
		count, status := service.namespace.Write(request.Handle, request.Data)
		reply.Status, reply.Size = status, uint64(count)
	case vfs.OperationSeek:
		reply.Status = service.namespace.Seek(request.Handle, request.Offset)
	case vfs.OperationCreate, vfs.OperationMkdir:
		reply.Status = service.namespace.Create(request.Path, request.Operation == vfs.OperationMkdir)
	case vfs.OperationReadDir:
		entry, cursor, status := service.namespace.ReadDir(request.Path, int(request.Offset))
		reply.Status, reply.Cursor = status, uint32(cursor)
		if status == vfs.StatusOK {
			reply.Kind, reply.Size, reply.Name = entry.Stat.Kind, entry.Stat.Size, entry.Name[:entry.Size]
		}
	case vfs.OperationRename:
		reply.Status = service.namespace.Rename(request.Path, request.Data)
	case vfs.OperationUnlink:
		reply.Status = service.namespace.Unlink(request.Path)
	case vfs.OperationStat:
		stat, status := service.namespace.Stat(request.Path)
		reply.Status, reply.Kind, reply.Size = status, stat.Kind, stat.Size
	case vfs.OperationTruncate:
		reply.Status = service.namespace.Truncate(request.Path, request.Offset)
	default:
		reply.Status = vfs.StatusInvalidArgument
	}
	return reply
}
