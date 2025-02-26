import { parseISO } from 'date-fns'
import { formatDistance } from 'date-fns/esm'
import { isArray, isEqual } from 'lodash'
import AlertCircleIcon from 'mdi-react/AlertCircleIcon'
import CheckCircleIcon from 'mdi-react/CheckCircleIcon'
import CheckIcon from 'mdi-react/CheckIcon'
import ErrorIcon from 'mdi-react/ErrorIcon'
import InformationIcon from 'mdi-react/InformationIcon'
import ProgressClockIcon from 'mdi-react/ProgressClockIcon'
import TimerSandIcon from 'mdi-react/TimerSandIcon'
import React, { useMemo } from 'react'
import { delay, distinctUntilChanged, repeatWhen } from 'rxjs/operators'

import { LoadingSpinner } from '@sourcegraph/react-loading-spinner'
import { Link } from '@sourcegraph/shared/src/components/Link'
import { BatchSpecExecutionState } from '@sourcegraph/shared/src/graphql-operations'
import { isDefined } from '@sourcegraph/shared/src/util/types'
import { useObservable } from '@sourcegraph/shared/src/util/useObservable'
import { Container, PageHeader } from '@sourcegraph/wildcard'

import { BatchChangesIcon } from '../../../batches/icons'
import { ErrorAlert } from '../../../components/alerts'
import { ExecutionLogEntry } from '../../../components/ExecutionLogEntry'
import { HeroPage } from '../../../components/HeroPage'
import { PageTitle } from '../../../components/PageTitle'
import { Timeline, TimelineStage } from '../../../components/Timeline'
import { BatchSpecExecutionFields, Scalars } from '../../../graphql-operations'
import { BatchSpec } from '../BatchSpec'

import { fetchBatchSpecExecution as _fetchBatchSpecExecution } from './backend'

export interface BatchSpecExecutionDetailsPageProps {
    executionID: Scalars['ID']

    /** For testing only. */
    fetchBatchSpecExecution?: typeof _fetchBatchSpecExecution
    /** For testing only. */
    now?: () => Date
    /** For testing only. */
    expandStage?: string
}

export const BatchSpecExecutionDetailsPage: React.FunctionComponent<BatchSpecExecutionDetailsPageProps> = ({
    executionID,
    now = () => new Date(),
    fetchBatchSpecExecution = _fetchBatchSpecExecution,
    expandStage,
}) => {
    const batchSpecExecution: BatchSpecExecutionFields | null | undefined = useObservable(
        useMemo(
            () =>
                fetchBatchSpecExecution(executionID).pipe(
                    repeatWhen(notifier => notifier.pipe(delay(2500))),
                    distinctUntilChanged((a, b) => isEqual(a, b))
                ),
            [fetchBatchSpecExecution, executionID]
        )
    )

    // Is loading.
    if (batchSpecExecution === undefined) {
        return (
            <div className="text-center">
                <LoadingSpinner className="icon-inline mx-auto my-4" />
            </div>
        )
    }
    // Is not found.
    if (batchSpecExecution === null) {
        return <HeroPage icon={AlertCircleIcon} title="Execution not found" />
    }

    return (
        <>
            <PageTitle title="Batch spec execution" />
            <PageHeader
                path={[
                    {
                        icon: BatchChangesIcon,
                        to: '/batch-changes',
                    },
                    {
                        to: `${batchSpecExecution.namespace.url}/batch-changes`,
                        text: batchSpecExecution.namespace.namespaceName,
                    },
                    { text: 'Execution' },
                ]}
                className="mb-3"
            />

            {batchSpecExecution.failure && <ErrorAlert error={batchSpecExecution.failure} />}

            <h2>Input spec</h2>
            <Container className="mb-3">
                <BatchSpec originalInput={batchSpecExecution.inputSpec} />
            </Container>

            <h2>Timeline</h2>
            <ExecutionTimeline execution={batchSpecExecution} now={now} expandStage={expandStage} className="mb-3" />

            {batchSpecExecution.batchSpec && (
                <>
                    <h2>Execution result</h2>
                    <div className="alert alert-info d-flex justify-space-between align-items-center">
                        <span className="flex-grow-1">Batch spec has been created.</span>
                        <Link to={batchSpecExecution.batchSpec.applyURL} className="btn btn-primary">
                            Preview changes
                        </Link>
                    </div>
                </>
            )}
        </>
    )
}

interface ExecutionTimelineProps {
    execution: BatchSpecExecutionFields
    className?: string

    /** For testing only. */
    now?: () => Date
    expandStage?: string
}

const ExecutionTimeline: React.FunctionComponent<ExecutionTimelineProps> = ({
    execution,
    className,
    now,
    expandStage,
}) => {
    const stages = useMemo(
        () => [
            { icon: <TimerSandIcon />, text: 'Queued', date: execution.createdAt, className: 'bg-success' },
            {
                icon: <CheckIcon />,
                text: 'Began processing',
                date: execution.startedAt,
                className: 'bg-success',
            },

            setupStage(execution, expandStage === 'setup', now),
            batchPreviewStage(execution, expandStage === 'srcPreview', now),
            teardownStage(execution, expandStage === 'teardown', now),

            execution.state === BatchSpecExecutionState.COMPLETED
                ? { icon: <CheckIcon />, text: 'Finished', date: execution.finishedAt, className: 'bg-success' }
                : { icon: <ErrorIcon />, text: 'Failed', date: execution.finishedAt, className: 'bg-danger' },
        ],
        [execution, now, expandStage]
    )
    return <Timeline stages={stages.filter(isDefined)} now={now} className={className} />
}

const setupStage = (
    execution: BatchSpecExecutionFields,
    expand: boolean,
    now?: () => Date
): TimelineStage | undefined =>
    execution.steps.setup.length === 0
        ? undefined
        : {
              text: 'Setup',
              details: execution.steps.setup.map(logEntry => (
                  <ExecutionLogEntry key={logEntry.key} logEntry={logEntry} now={now} />
              )),
              ...genericStage(execution.steps.setup, expand),
          }

const batchPreviewStage = (
    execution: BatchSpecExecutionFields,
    expand: boolean,
    now?: () => Date
): TimelineStage | undefined =>
    !execution.steps.srcPreview
        ? undefined
        : {
              text: 'Create batch spec preview',
              details: (
                  <ExecutionLogEntry logEntry={execution.steps.srcPreview} now={now}>
                      {execution.steps.srcPreview.out && <ParsedJsonOutput out={execution.steps.srcPreview.out} />}
                  </ExecutionLogEntry>
              ),
              ...genericStage(execution.steps.srcPreview, expand),
          }

const teardownStage = (
    execution: BatchSpecExecutionFields,
    expand: boolean,
    now?: () => Date
): TimelineStage | undefined =>
    execution.steps.teardown.length === 0
        ? undefined
        : {
              text: 'Teardown',
              details: execution.steps.teardown.map(logEntry => (
                  <ExecutionLogEntry key={logEntry.key} logEntry={logEntry} now={now} />
              )),
              ...genericStage(execution.steps.teardown, expand),
          }

const genericStage = <E extends { startTime: string; exitCode: number | null }>(
    value: E | E[],
    expand: boolean
): Pick<TimelineStage, 'icon' | 'date' | 'className' | 'expanded'> => {
    const finished = isArray(value) ? value.every(logEntry => logEntry.exitCode !== null) : value.exitCode !== null
    const success = isArray(value) ? value.every(logEntry => logEntry.exitCode === 0) : value.exitCode === 0

    return {
        icon: !finished ? <ProgressClockIcon /> : success ? <CheckIcon /> : <ErrorIcon />,
        date: isArray(value) ? value[0].startTime : value.startTime,
        className: success || !finished ? 'bg-success' : 'bg-danger',
        expanded: expand || !(success || !finished),
    }
}

enum JSONLogLineOperation {
    PARSING_BATCH_SPEC = 'PARSING_BATCH_SPEC',
    RESOLVING_NAMESPACE = 'RESOLVING_NAMESPACE',
    PREPARING_DOCKER_IMAGES = 'PREPARING_DOCKER_IMAGES',
    DETERMINING_WORKSPACE_TYPE = 'DETERMINING_WORKSPACE_TYPE',
    RESOLVING_REPOSITORIES = 'RESOLVING_REPOSITORIES',
    DETERMINING_WORKSPACES = 'DETERMINING_WORKSPACES',
    CHECKING_CACHE = 'CHECKING_CACHE',
    EXECUTING_TASKS = 'EXECUTING_TASKS',
    EXECUTING_TASK = 'EXECUTING_TASK',
    UPLOADING_CHANGESET_SPECS = 'UPLOADING_CHANGESET_SPECS',
    CREATING_BATCH_SPEC = 'CREATING_BATCH_SPEC',
}

const prettyOperationNames: Record<JSONLogLineOperation, string> = {
    PARSING_BATCH_SPEC: 'Parsing batch spec',
    RESOLVING_NAMESPACE: 'Resolving namespace',
    PREPARING_DOCKER_IMAGES: 'Preparing docker images',
    DETERMINING_WORKSPACE_TYPE: 'Determining workspace type',
    RESOLVING_REPOSITORIES: 'Resolving repositories',
    DETERMINING_WORKSPACES: 'Determining workspaces',
    CHECKING_CACHE: 'Checking cache',
    EXECUTING_TASKS: 'Executing tasks',
    EXECUTING_TASK: 'Executing task',
    UPLOADING_CHANGESET_SPECS: 'Uploading changeset specs',
    CREATING_BATCH_SPEC: 'Creating batch spec',
}

enum JSONLogLineStatus {
    STARTED = 'STARTED',
    PROGRESS = 'PROGRESS',
    SUCCESS = 'SUCCESS',
    FAILED = 'FAILED',
}

interface ExecutingTaskJSONLogLine {
    operation: JSONLogLineOperation.EXECUTING_TASK
    timestamp: string
    status: JSONLogLineStatus
    message?: string
    metadata: {
        task: Task
    }
}

type JSONLogLine =
    | {
          operation: JSONLogLineOperation
          timestamp: string
          status: JSONLogLineStatus
          message?: string
          metadata: {
              task?: Task
              tasks?: Task[]
          }
      }
    | ExecutingTaskJSONLogLine

interface Step {
    run: string
    container: string
}

interface Task {
    Repository: string
    Workspace: string
    Steps: Step[]
    CachedStepResultsFound: boolean
}

const ParsedJsonOutput: React.FunctionComponent<{ out: string }> = ({ out }) => {
    const parsed = useMemo<JSONLogLine[]>(
        () =>
            out
                .split('\n')
                .map(line => line.replace(/^std(out|err): /, ''))
                .map(line => {
                    try {
                        return JSON.parse(line) as JSONLogLine
                    } catch (error) {
                        return String(error)
                    }
                })
                .filter((line): line is JSONLogLine => typeof line !== 'string'),
        [out]
    )

    const parsedExecutingTaskLines = useMemo<ExecutingTaskJSONLogLine[]>(
        () =>
            parsed.filter(
                (line): line is ExecutingTaskJSONLogLine => line.operation === JSONLogLineOperation.EXECUTING_TASK
            ),
        [parsed]
    )

    return (
        <ul className="list-group w-100 mt-3">
            {Object.values<JSONLogLineOperation>(JSONLogLineOperation).map(operation => {
                const tuple = findLogLineTuple(parsed, operation)
                if (tuple === undefined) {
                    return null
                }
                const completionStatus = tuple[1]?.status
                return (
                    <li className="list-group-item p-2" key={operation}>
                        <div className="d-flex justify-content-between">
                            <p>
                                {completionStatus === JSONLogLineStatus.SUCCESS && (
                                    <CheckCircleIcon className="icon-inline text-success mr-1" />
                                )}
                                {completionStatus === JSONLogLineStatus.FAILED && (
                                    <ErrorIcon className="icon-inline text-danger mr-1" />
                                )}
                                {prettyOperationNames[tuple[0].operation]}
                            </p>
                            <span>
                                {formatDistance(
                                    parseISO(tuple[0].timestamp),
                                    parseISO(tuple[1]?.timestamp ?? new Date().toISOString()),
                                    { includeSeconds: true }
                                )}
                            </span>
                        </div>
                        {operation === JSONLogLineOperation.EXECUTING_TASKS && (
                            <ParsedTaskExecutionOutput lines={parsedExecutingTaskLines} />
                        )}
                        <code className="d-block">
                            {[tuple[0].message, tuple[1]?.message].filter(line => !!line).join('\n')}
                        </code>
                    </li>
                )
            })}
        </ul>
    )
}

const ParsedTaskExecutionOutput: React.FunctionComponent<{ lines: ExecutingTaskJSONLogLine[] }> = ({ lines }) => (
    <ul className="list-group w-100 mt-3">
        {lines.map((line, index) => {
            const repo = line.metadata.task.Repository
            const key = `${repo}-${index}`

            if (line.status === JSONLogLineStatus.STARTED) {
                return (
                    <li className="list-group-item p-2" key={key}>
                        <InformationIcon className="icon-inline mr-1" />
                        <b>{repo}</b>: Starting execution of {line.metadata?.task?.Steps?.length}
                    </li>
                )
            }
            if (line.status === JSONLogLineStatus.SUCCESS) {
                return (
                    <li className="list-group-item p-2" key={key}>
                        <CheckCircleIcon className="icon-inline text-success mr-1" />
                        <b>{repo}</b>: Success! All steps executed.
                    </li>
                )
            }
            if (line.status === JSONLogLineStatus.FAILED) {
                return (
                    <li className="list-group-item p-2" key={key}>
                        <ErrorIcon className="icon-inline text-danger mr-1" />
                        <b>{repo}</b>: Failed :(
                    </li>
                )
            }
            return (
                <li className="list-group-item p-2" key={key}>
                    <b>{repo}</b>: <code>{line.message}</code>
                </li>
            )
        })}
    </ul>
)

function findLogLine(
    lines: JSONLogLine[],
    operation: JSONLogLineOperation,
    status: JSONLogLineStatus
): JSONLogLine | undefined {
    return lines.find(line => line.operation === operation && line.status === status)
}

function findLogLineTuple(
    lines: JSONLogLine[],
    operation: JSONLogLineOperation
): [JSONLogLine] | [JSONLogLine, JSONLogLine] | undefined {
    const start = findLogLine(lines, operation, JSONLogLineStatus.STARTED)
    if (!start) {
        return undefined
    }
    let end = findLogLine(lines, operation, JSONLogLineStatus.SUCCESS)
    if (!end) {
        end = findLogLine(lines, operation, JSONLogLineStatus.FAILED)
    }
    if (end) {
        return [start, end]
    }
    return [start]
}
