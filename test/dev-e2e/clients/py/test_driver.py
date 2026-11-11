"""The raw-result probe must reject equal-but-wrong transport responses."""

import asyncio
import importlib.util
from pathlib import Path

import grpc
import pytest
import tipsy_ab_config as tac
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

spec = importlib.util.spec_from_file_location("raw_probe_driver", Path(__file__).with_name("run.py"))
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)


class Rejected(grpc.RpcError):
    def __init__(self, valid):
        self.status_code = 400 if valid else 500
        self.valid = valid

    def code(self):
        return grpc.StatusCode.FAILED_PRECONDITION if self.valid else grpc.StatusCode.INTERNAL

    def __str__(self):
        return "custom_params is retired"


@pytest.mark.parametrize("client", ["py_sdk_http", "py_sdk_grpc"])
@pytest.mark.parametrize("case", ["hit", "all_empty", "wrong_user", "wrong_error_code", "config_has_custom", "no_hit", "missing_golden"])
def test_raw_probe_checks_golden_and_rejection_code(client, case):
    class FakeClient:
        async def get_experiment_result(self, namespace, *, user_info, experiment_type=0):
            assert namespace == "probe"
            assert user_info.experiment_hash_id == "u"
            if experiment_type == 2:
                raise Rejected(case != "wrong_error_code")
            values = {"feature": 123}
            if case in ("all_empty", "no_hit", "missing_golden"):
                values = {}
            elif case == "wrong_user":
                values = {"feature": 124}
            response = abtest_pb2.GetExperimentResultResponse(config_flat_kv=values)
            if case == "config_has_custom" and experiment_type == 1:
                response.custom_flat_kv.update({"secret": "retired"})
            return response

    expectation = {"ns": "probe", "user_id": "u", "user_attrs": {}, "key": "__custom__"}
    if case != "missing_golden":
        expectation["expected_config_flat_kv"] = {} if case == "no_hit" else {"feature": "123"}
    result = driver.Results()
    asyncio.run(driver.assert_expectation(FakeClient(), client, expectation, result, tac, abtest_pb2))
    if case in ("hit", "no_hit"):
        assert (result.passed, result.failed) == (1, 0)
    else:
        assert (result.passed, result.failed) == (0, 1)
