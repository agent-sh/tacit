#!/usr/bin/env python3
"""Show guards fail under deliberately unsafe implementations, without editing source."""
import io
import unittest
import test_harness as tests

real=tests.context_query

def naive(case, arm):
    # Deliberately unsafe history expansion and whole-record serialization.
    import json
    return dict(original=case['prompt'], query=case['prompt']+'\n'+json.dumps(case), sources=[], arm=arm)

tests.context_query=naive
names=['test_original_identifier_literal','test_topic_switch','test_null','test_labels_and_assistant_never_enter_context']
result=unittest.TextTestRunner(stream=io.StringIO()).run(unittest.TestSuite(tests.ContractTests(n) for n in names))
assert len(result.failures)==len(names), result
print('Unsafe context mutant: all four identifier/topic/null/leakage tests failed as required.')
tests.context_query=real
result=unittest.TextTestRunner(verbosity=1).run(unittest.TestSuite(tests.ContractTests(n) for n in names))
assert result.wasSuccessful()
