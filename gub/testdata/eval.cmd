# Test of eval
# Use with expr.go
set highlight off
next
next
next
# Should be able to see expr
eval expr + " foo "
# -2
eval -2
# 5 == 6
eval 5 == 6
# 5 < 6
eval 5 < 6
# 1 << n
eval 1 << n
## FIXME: reinstate
## 1 << 8
## eval 1 << 8
# y(
eval y(
# exprs
exprs
# eval exprs[0]
eval exprs[0]
# eval exprs[100]
eval exprs[100]
# eval exprs[-9]
eval exprs[-9]
# eval os.O_RDWR | 4
eval os.O_RDWR | 4
# eval os.Args
eval os.Args
# eval os.Args[0]
eval os.Args[0]
# eval "we have: " + exprs[5] + "."
eval "we have: " + exprs[5] + "."
# eval len("abc") # -- builtin len() with string
eval len("abc")
# eval len(exprs) # -- builtin len() with array
eval len(exprs)
# eval fmt.Println("Hi there!") # -- Eval package fn
eval fmt.Println("Hi there!")
## FIXME eval should handle types better
## Shouldn't need the int(20) below
# eval strconv.Atoi("13") + int(20) # -- Eval package fn expression
eval strconv.Atoi("13") + int(20)
quit
