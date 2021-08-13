import { storiesOf } from '@storybook/react'
import React, { useState, useCallback } from 'react'
import { Popover } from 'reactstrap'

import { Button } from '@sourcegraph/wildcard'

import { WebStory } from '../../components/WebStory'

import { RevisionsPopover } from './RevisionsPopover'
import { MOCK_PROPS, MOCK_REQUESTS } from './RevisionsPopover.mocks'

const { add } = storiesOf('web/RevisionsPopover', module).addDecorator(story => (
    <div className="p-3 container">{story()}</div>
))

add('Full customizable', () => {
    const [popoverOpen, setPopoverOpen] = useState(false)
    const togglePopover = useCallback(() => setPopoverOpen(previous => !previous), [])

    return (
        <WebStory mocks={MOCK_REQUESTS} initialEntries={[{ pathname: `/${MOCK_PROPS.repoName}` }]}>
            {() => (
                <>
                    <Button variant="primary" id="popover-trigger">
                        Click me!
                    </Button>
                    <Popover
                        isOpen={popoverOpen}
                        toggle={togglePopover}
                        placement="bottom-start"
                        target="popover-trigger"
                        trigger="legacy"
                        hideArrow={true}
                        fade={false}
                    >
                        <RevisionsPopover {...MOCK_PROPS} />
                    </Popover>
                </>
            )}
        </WebStory>
    )
})
