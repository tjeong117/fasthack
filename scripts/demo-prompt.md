You are working in a checkout of SymPy. A regression has been reported in
`sympy/core`.

Do the following, in this order:

1. Reproduce the current state of the core test suite by running EXACTLY these
   three commands, one at a time, and reading the output of each:

   python -m pytest -q -p no:cacheprovider sympy/core/tests/test_expr.py
   python -m pytest -q -p no:cacheprovider sympy/core/tests/test_arit.py
   python -m pytest -q -p no:cacheprovider sympy/core/tests/test_numbers.py

2. Report which tests fail, if any, and what the failure says.

3. If a test fails, find the cause in the source and fix it. If nothing fails,
   say so and stop.

Run the three commands above verbatim. Do not add flags, do not change the
paths, and do not combine them into one invocation.

---

WHY THE VERBATIM INSTRUCTION IS THERE, for us and not for the agent:

The cache keys on the normalized command string, so `pytest x.py` and
`python -m pytest x.py` and `python -m pytest -q x.py` are three different
keys and share nothing. Left to themselves, five agents phrase the same
intention five different ways and the measured hit rate collapses for a reason
that has nothing to do with whether caching works.

Pinning the commands is not cheating the benchmark, it is how a fan-out is
actually operated: when you dispatch N agents at one bug you give them the
repro command. But it IS a thing to disclose, so disclose it.

`-p no:cacheprovider` is load-bearing. Without it pytest writes `.pytest_cache/`,
sympy does not gitignore that, the tree hash changes, the purity gate fails,
and every record is unservable.
