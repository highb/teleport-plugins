package lib

type StringSet map[string]struct{}

func (set StringSet) Add(str string) {
	set[str] = struct{}{}
}

func (set StringSet) Del(str string) {
	delete(set, str)
}

func (set StringSet) Len() int {
	return len(set)
}

func (set StringSet) Contains(str string) bool {
	_, ok := set[str]
	return ok
}
