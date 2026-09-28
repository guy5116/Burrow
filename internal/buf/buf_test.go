package buf

import "testing"

func TestPool(t *testing.T) {
	b := Get()
	if len(*b) != Size {
		t.Fatalf("len %d", len(*b))
	}
	(*b)[0] = 7
	Put(b)
	Put(nil)
	small := make([]byte, 3)
	Put(&small) // ignored
	c := Get()
	for i := range *c {
		if (*c)[i] != 0 {
			t.Fatal("buffer not wiped")
		}
	}
	Put(c)
	if n := testing.AllocsPerRun(100, func() { Put(Get()) }); n != 0 {
		t.Fatalf("allocs per Get/Put = %v", n)
	}
}
