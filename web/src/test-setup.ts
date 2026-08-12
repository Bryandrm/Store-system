// A real IndexedDB implementation, in memory. Same reasoning as the Go side:
// no mocks, because a mocked store cannot reproduce transaction semantics.
import 'fake-indexeddb/auto'
