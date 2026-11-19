// Port of Go's sort.Slice (pdqsort_func) so unstable orderings match the Go helper exactly.

struct D<'a, T, F: Fn(&T, &T) -> bool> {
    v: &'a mut [T],
    less: F,
}

impl<'a, T, F: Fn(&T, &T) -> bool> D<'a, T, F> {
    fn less(&self, i: usize, j: usize) -> bool {
        (self.less)(&self.v[i], &self.v[j])
    }
    fn swap(&mut self, i: usize, j: usize) {
        self.v.swap(i, j)
    }
}

pub fn sort_slice<T, F: Fn(&T, &T) -> bool>(v: &mut [T], less: F) {
    let n = v.len();
    let limit = (usize::BITS - n.leading_zeros()) as usize;
    let mut d = D { v, less };
    pdqsort(&mut d, 0, n, limit);
}

fn insertion_sort<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize) {
    for i in a + 1..b {
        let mut j = i;
        while j > a && d.less(j, j - 1) {
            d.swap(j, j - 1);
            j -= 1;
        }
    }
}

fn sift_down<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, lo: usize, hi: usize, first: usize) {
    let mut root = lo;
    loop {
        let mut child = 2 * root + 1;
        if child >= hi {
            return;
        }
        if child + 1 < hi && d.less(first + child, first + child + 1) {
            child += 1;
        }
        if !d.less(first + root, first + child) {
            return;
        }
        d.swap(first + root, first + child);
        root = child;
    }
}

fn heap_sort<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize) {
    let first = a;
    let hi = b - a;
    let mut i = (hi as isize - 1) / 2;
    while i >= 0 {
        sift_down(d, i as usize, hi, first);
        i -= 1;
    }
    let mut i = hi as isize - 1;
    while i >= 0 {
        d.swap(first, first + i as usize);
        sift_down(d, 0, i as usize, first);
        i -= 1;
    }
}

#[derive(PartialEq, Clone, Copy)]
enum Hint {
    Unknown,
    Increasing,
    Decreasing,
}

fn pdqsort<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, mut a: usize, mut b: usize, mut limit: usize) {
    let mut was_balanced = true;
    let mut was_partitioned = true;
    loop {
        let length = b - a;
        if length <= 12 {
            insertion_sort(d, a, b);
            return;
        }
        if limit == 0 {
            heap_sort(d, a, b);
            return;
        }
        if !was_balanced {
            break_patterns(d, a, b);
            limit -= 1;
        }
        let (mut pivot, mut hint) = choose_pivot(d, a, b);
        if hint == Hint::Decreasing {
            reverse_range(d, a, b);
            pivot = (b - 1) - (pivot - a);
            hint = Hint::Increasing;
        }
        if was_balanced && was_partitioned && hint == Hint::Increasing && partial_insertion_sort(d, a, b) {
            return;
        }
        if a > 0 && !d.less(a - 1, pivot) {
            a = partition_equal(d, a, b, pivot);
            continue;
        }
        let (mid, already) = partition(d, a, b, pivot);
        was_partitioned = already;
        let (left, right) = (mid - a, b - mid);
        let threshold = length / 8;
        if left < right {
            was_balanced = left >= threshold;
            pdqsort(d, a, mid, limit);
            a = mid + 1;
        } else {
            was_balanced = right >= threshold;
            pdqsort(d, mid + 1, b, limit);
            b = mid;
        }
    }
}

fn partition<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize, pivot: usize) -> (usize, bool) {
    d.swap(a, pivot);
    let (mut i, mut j) = (a as isize + 1, b as isize - 1);
    while i <= j && d.less(i as usize, a) {
        i += 1;
    }
    while i <= j && !d.less(j as usize, a) {
        j -= 1;
    }
    if i > j {
        d.swap(j as usize, a);
        return (j as usize, true);
    }
    d.swap(i as usize, j as usize);
    i += 1;
    j -= 1;
    loop {
        while i <= j && d.less(i as usize, a) {
            i += 1;
        }
        while i <= j && !d.less(j as usize, a) {
            j -= 1;
        }
        if i > j {
            break;
        }
        d.swap(i as usize, j as usize);
        i += 1;
        j -= 1;
    }
    d.swap(j as usize, a);
    (j as usize, false)
}

fn partition_equal<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize, pivot: usize) -> usize {
    d.swap(a, pivot);
    let (mut i, mut j) = (a as isize + 1, b as isize - 1);
    loop {
        while i <= j && !d.less(a, i as usize) {
            i += 1;
        }
        while i <= j && d.less(a, j as usize) {
            j -= 1;
        }
        if i > j {
            break;
        }
        d.swap(i as usize, j as usize);
        i += 1;
        j -= 1;
    }
    i as usize
}

fn partial_insertion_sort<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize) -> bool {
    let mut i = a + 1;
    for _ in 0..5 {
        while i < b && !d.less(i, i - 1) {
            i += 1;
        }
        if i == b {
            return true;
        }
        if b - a < 50 {
            return false;
        }
        d.swap(i, i - 1);
        if i - a >= 2 {
            let mut j = i - 1;
            while j >= 1 {
                if !d.less(j, j - 1) {
                    break;
                }
                d.swap(j, j - 1);
                j -= 1;
            }
        }
        if b - i >= 2 {
            for j in i + 1..b {
                if !d.less(j, j - 1) {
                    break;
                }
                d.swap(j, j - 1);
            }
        }
    }
    false
}

fn break_patterns<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize) {
    let length = b - a;
    if length >= 8 {
        let mut r = length as u64;
        let modulus: u64 = 1u64 << (u64::BITS - (length as u64).leading_zeros());
        let idx = a + (length / 4) * 2 - 1;
        for i in 0..3 {
            r ^= r << 13;
            r ^= r >> 7;
            r ^= r << 17;
            let mut other = (r & (modulus - 1)) as usize;
            if other >= length {
                other -= length;
            }
            d.swap(idx - 1 + i, a + other);
        }
    }
}

fn choose_pivot<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize) -> (usize, Hint) {
    let l = b - a;
    let mut swaps = 0;
    let mut i = a + l / 4;
    let mut j = a + l / 4 * 2;
    let mut k = a + l / 4 * 3;
    if l >= 8 {
        if l >= 50 {
            i = median(d, i - 1, i, i + 1, &mut swaps);
            j = median(d, j - 1, j, j + 1, &mut swaps);
            k = median(d, k - 1, k, k + 1, &mut swaps);
        }
        j = median(d, i, j, k, &mut swaps);
    }
    match swaps {
        0 => (j, Hint::Increasing),
        12 => (j, Hint::Decreasing),
        _ => (j, Hint::Unknown),
    }
}

fn order2<T, F: Fn(&T, &T) -> bool>(d: &D<T, F>, a: usize, b: usize, swaps: &mut i32) -> (usize, usize) {
    if d.less(b, a) {
        *swaps += 1;
        return (b, a);
    }
    (a, b)
}

fn median<T, F: Fn(&T, &T) -> bool>(d: &D<T, F>, a: usize, b: usize, c: usize, swaps: &mut i32) -> usize {
    let (a, b) = order2(d, a, b, swaps);
    let (b, c) = order2(d, b, c, swaps);
    let _ = c;
    let (_a, b) = order2(d, a, b, swaps);
    b
}

fn reverse_range<T, F: Fn(&T, &T) -> bool>(d: &mut D<T, F>, a: usize, b: usize) {
    let (mut i, mut j) = (a, b - 1);
    while i < j {
        d.swap(i, j);
        i += 1;
        j -= 1;
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn sorts() {
        let mut v: Vec<i64> = (0..500).map(|i| (i * 7919) % 101).collect();
        super::sort_slice(&mut v, |a, b| a < b);
        assert!(v.windows(2).all(|w| w[0] <= w[1]));
    }
}
