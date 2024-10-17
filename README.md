# StreamHive

StreamHive is a distributed, content-addressed file storage experiment in Go. The long-term goal is decentralized chunk storage and replication; the current codebase provides a tested TCP peer transport as the networking foundation.

**Status:** networking layer (listen, accept, dial, graceful close). Storage and content addressing are not implemented yet.
