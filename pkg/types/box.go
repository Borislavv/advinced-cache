package types

type SizedBox[T any] struct {
	Value        T
	CalcWeightFn func(s *SizedBox[T]) int64
}

func NewSizedBox[T any](value T, calcWeightFn func(s *SizedBox[T]) int64) *SizedBox[T] {
	return &SizedBox[T]{
		Value:        value,
		CalcWeightFn: calcWeightFn,
	}
}

func (s *SizedBox[T]) Weight() int64 {
	return s.CalcWeightFn(s)
}
